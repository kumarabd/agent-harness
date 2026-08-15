# Component: Gateway
## (Per-Platform Ingestion & Delivery)

> STATUS: IN PROGRESS — core flow, deployment model, and reliability model resolved. A few naming/technology details remain open.

### Role (one line)
The platform-integration boundary: normalizes whatever a messaging platform sends into a generic `MessageEvent`, submits it durably into Temporal, and (for connection-based platforms) delivers the eventual response back out over the same live connection. Deliberately platform-generic engineering — nothing here is agent-specific; it's the same layer any chat-connected system would need.

### Why one gateway *type* per platform, not one process per channel
Originally framed as "one gateway per source" meaning one process per channel/chat. Revised: the deployment unit is **one gateway type per platform** (`DiscordGateway`, `WhatsAppGateway`, ...), each independently horizontally scaled — internally handling many users/channels/tenants at once, not one process per conversation. This is the more correct generalization of the same underlying principle ("a conversation should only ever be reachable through one coordinated path"), because the right replication strategy differs fundamentally by platform category:

- **Connection-based platforms** (Discord, Slack Socket Mode, Signal) hold a live, stateful socket. The platform itself already partitions traffic deterministically (e.g. Discord shards by `guild_id % num_shards`) — replicas must mirror that partitioning, not round-robin.
- **Webhook-based platforms** (WhatsApp Business API, Telegram, Slack Events API) have no persistent connection — inbound arrives as a stateless HTTP POST, outbound is a stateless HTTP call. No partitioning concept exists; replicas are fully interchangeable.

### Resolved: Always Async — No Sync-Required Platforms Supported
Even platforms that *offer* a synchronous response path (e.g. Slack slash commands, technically answerable within the same request) are deliberately treated as async here: ack fast, deliver the real answer later via whatever async mechanism the platform provides (a callback URL, a separate outbound API, the platform's own live connection). Holding an HTTP request or blocking a thread open for the duration of a full agentic turn (possibly involving deep tool-call sequences or subagent recursion) doesn't scale and doesn't fit this design's turn-duration profile.

**Explicit scope boundary:** a platform with *no* async mechanism at all — the full final answer must be returned on the same inbound request, no callback URL, no separate send API — is simply **not supported** by this gateway design. This was considered (a request-scoped Postgres `LISTEN`/`NOTIFY` wait with a bounded timeout was designed as a possible mechanism) and explicitly rejected in favor of keeping the gateway uniformly async. Stated plainly so it isn't rediscovered as a surprise later.

### Resolved: Inbound Flow
1. Raw event arrives at a gateway replica — via its own websocket (connection-based, only reaches the replica owning that shard) or via HTTP POST (webhook-based, any replica, load-balanced).
2. Normalize into a `MessageEvent` — the generic/agentic boundary. Nothing past this step is platform-specific.
3. Resolve `session_key`, run the auth check.
4. **Dedup check-then-insert against `ingested_messages(platform, platform_message_id)`** — a ledger symmetric to `delivered_responses` on the outbound side. Needed because webhook platforms redeliver at-least-once; without this, a redelivered webhook would double-signal. If already present, skip to step 6 (ack only) — the message was already durably submitted.
5. `SignalWithStart` the session coordinator (workflow ID = `session_key`), passing the message content as the signal payload. This is the durable submission — Temporal records the payload in workflow history, so **the gateway does not separately write the message body to Postgres.** The full-content persist into `messages` happens later, as an activity inside the coordinator/turn flow, sourced from the signal payload — not duplicated at the gateway. (This removes a real redundancy an earlier version of this design had: writing the same payload durably twice, once to Postgres directly and once via the signal.)
6. Ack the platform (webhook: return 200 now; connection-based: no separate ack, just don't block).
7. Gateway's job for this message ends here — no correlation state kept, no waiting on the turn to finish. This is what "always async" buys structurally: nothing pending, nothing to garbage-collect if the gateway restarts.

*(Slash commands like `/approve`/`/stop` still route differently — unresolved, unchanged by this design.)*

### Resolved: Outbound Flow — Delivery as a Colocated/Separate Temporal Worker, Not a Broker
**Superseded design choice:** the original plan used a message broker (Redis Streams/NATS) as an "outbound queue" between the deliver activity and the gateway. Reconsidered and dropped — it solved a problem created by assuming delivery runs on a generic worker pool physically separate from the gateway. A simpler mechanism removes the need for a broker entirely:

- **The gateway process itself runs an embedded Temporal worker**, registered for `DeliverActivity` on a task queue specific to what it owns.
- The turn workflow, at delivery time, computes the target task queue **deterministically** — by shard (connection-based: same formula used for inbound partitioning, e.g. `guild_id % num_shards` → a shard-specific queue name) or by platform alone (webhook-based: one shared queue, any replica polling it can serve it).
- `ExecuteActivity(DeliverActivity, response, { taskQueue: computed_queue })` — Temporal's own task dispatch delivers the activity task to whichever gateway replica is polling that queue. Retry, durability, and correct targeting all come from Temporal's existing activity mechanics — the same retry policy already defined in `components/activities-outbound-delivery.md`, no new system needed.
- This directly resolves two items that were previously open ("outbound queue technology choice", "how deliver addresses the exact owning gateway instance") by making them the same already-solved problem as shard addressing, rather than a separate one.
- The `delivered_responses` idempotency ledger (`components/activities-outbound-delivery.md`) is unchanged: the receiving replica still checks-then-inserts before making the real platform send call, regardless of how the activity task reached it.

**Platform-dependent worker placement — this is a real, inherent split, not a uniform choice:**
- **Webhook-based platforms:** the delivery worker is a fully separate, generic, unpinned pool — no coupling to any specific gateway replica. Any worker polling the shared queue can make the outbound HTTP call.
- **Connection-based platforms:** the send call must go out over the *exact* live socket that owns that shard — a physical constraint (a TCP connection is memory-resident to one process), not a design preference. So the `DeliverActivity` worker for these platforms must be **colocated inside the gateway replica that holds that shard's connection** — same process, same deployment unit. It's still architecturally a normal Temporal worker/activity; "which worker" is just constrained to "the one holding this shard," not a free-floating pool.

### Resolved: Shard Assignment — Static Config + Orchestrator Identity, Not a Dynamic Lease
For connection-based platforms only (webhook platforms have no shard concept and need none of this):

- **Shard count** is platform-driven (e.g. Discord recommends a shard count based on guild count) and changes rarely — treated as a slow-changing capacity-planning number, not something to elastically rebalance at runtime.
- **Shard-to-replica assignment is static config**, not a runtime-negotiated fact: replica *i* is simply configured to own shard *i*. No lease table, no coordination protocol.
- **Deployed as a StatefulSet** (not a Deployment) specifically so pod ordinal → shard ID mapping is stable and free — this is the one piece of infrastructure doing real work here: identity survives pod restarts without any application-level coordination.
- **Delivery addressing is a pure computation from this same static config** (`shard_id = guild_id % num_shards` → known network address for that shard's pod), not a stored, potentially-stale fact. This makes `session_routing` (previously in `components/state-layer.md`) unnecessary — see that doc's notes log for the removal.

**Explicitly rejected: a dynamic Postgres lease table for shard ownership** (the `session_filesystem_leases` pattern applied to shards). Considered first, since it reuses an existing mechanism, but rejected once static config was recognized as sufficient — shard count doesn't change often enough to need runtime negotiation, and the StatefulSet's stable identity already gives crash-recovery (a replacement pod with the same ordinal reclaims the same shard automatically, no lease expiry/reclaim logic needed).

**Deployment model tradeoff, made explicit:**
- **Webhook-based platforms: stateless `Deployment`.** Pods are fully fungible, fits a `HorizontalPodAutoscaler` naturally if load ever warrants it.
- **Connection-based platforms: `StatefulSet`.** Costs some operational simplicity (StatefulSets restart pods one-at-a-time by default — slower rolling deploys than a Deployment) and forgoes uniformity of tooling across gateway types. In exchange, gets free static shard identity without a coordination mechanism. `HorizontalPodAutoscaler`'s main benefit — elastic scaling with load — doesn't apply much here anyway, since shard count tracks guild count, not request volume (same reasoning the topology doc already applies: a single conversation never generates enough volume to need elastic scaling). Given that, StatefulSet's cost is judged worth paying rather than reintroducing dynamic coordination just to use a uniform deployment kind.

### Resolved: Failure Analysis — What Happens When a Connection-Based Worker Dies Mid-Processing
Split into two genuinely different cases:

**1. Worker crashes before `SignalWithStart` succeeds (raw event received, not yet durably submitted).** This message is genuinely lost from our system's perspective — there is no mechanism that recovers it, because Temporal's durability guarantee only starts once something is inside Temporal, and nothing was written anywhere durable before the crash. This is an inherent boundary condition of bridging an external at-most-once event source into a durable internal system, not specific to this design. **What bounds the damage:** the platform's own protocol. Discord assigns each connection a sequence number and supports resuming a dropped session (replaying events since the last acknowledged sequence) rather than reconnecting cold. **Requirement this creates: gateway reconnection logic must prefer session-resume over fresh-reconnect wherever the platform supports it** — this is what actually closes most of the loss window, not anything in our own system.

**2. Worker crashes after `SignalWithStart` succeeds.** Fully covered by existing mechanisms, nothing new needed: the signal is durably in Temporal's workflow history regardless of the gateway worker's fate. The only consequence is a window where that shard has no active listener/delivery worker — inbound events for that shard during the gap follow case 1's story; outbound deliveries for that shard simply sit as pending Temporal activity tasks on that shard's task queue until the replacement pod (same ordinal, same shard identity via the StatefulSet) resumes polling — nothing is lost on that side, Temporal just holds the task.

**What does NOT help here, addressed directly:** writing the inbound message to Postgres at the gateway before signaling does not close the loss window — that write is just as vulnerable to a pre-submission crash as the signal itself, since both are downstream of the same gateway process being alive. The only thing genuinely upstream of a gateway crash is the platform's own durable record of what it already sent (the sequence-number/resume mechanism above) — which is also the reason the persist-to-Postgres step was moved off the gateway's inbound path entirely (see Inbound Flow, step 5) rather than kept as a redundant local safety net that doesn't actually add safety.

**Requirement this creates for `gateway_shard_state`:** to make session-resume actually work across a crash/restart, the last-acknowledged sequence number needs to survive the crash. This is a few bytes, not real session state — stored in Postgres (`components/state-layer.md`), not on local pod disk. Local StatefulSet disk was considered and rejected for this: it reintroduces the same "worker-pinned state that can still be lost" risk already rejected for the session filesystem (`components/session-filesystem.md`), for no benefit over a tiny, cheap, already-available Postgres table. StatefulSet identity is still exactly the right mechanism — just for *shard assignment*, not for storing data.

### Key Design Decisions (recap)
- One gateway *type* per platform, each independently horizontally scaled — not one process per channel.
- Always async — no platform requiring a same-request synchronous response is supported.
- Delivery is a Temporal worker (embedded in the gateway process), addressed via deterministic task-queue routing — not a message broker.
- Connection-based platforms: colocated delivery worker per shard, static config + StatefulSet identity for shard assignment, no dynamic lease table.
- Webhook-based platforms: fully separate, generic, stateless delivery worker pool; no shard concept.
- Inbound message loss (pre-signal crash) is real and platform-resume-bounded, not solvable by adding a redundant Postgres write at the gateway.

### Open Questions / To Design
- Exact `MessageEvent` schema.
- Auth model details.
- Exact task-queue naming scheme (illustrative names used above, e.g. `deliver:discord:shard:3`, not finalized).
- Slash command routing (`/approve`, `/deny`, `/stop`) now that execution has moved off the gateway process — still unresolved from earlier in this doc set.
- Connection/reconnection handling details per platform beyond the general resume-preference requirement (exact backoff, session-resume API specifics per platform).

### Notes Log
- 2026-08-07: Full gateway design resolved from scratch (previously a bare scaffold): platform-type-based deployment model, always-async scope boundary (sync-required platforms explicitly unsupported), task-queue-based delivery replacing the originally-planned outbound broker, static-config shard assignment replacing a considered-and-rejected dynamic lease table, StatefulSet-vs-Deployment tradeoff made explicit per platform category, and a two-case failure analysis establishing that inbound loss is real (platform-resume-bounded) while a redundant gateway-side Postgres write does not help. Introduces `ingested_messages` (inbound dedup ledger) and `gateway_shard_state` (last-sequence-number, for resume) as new state-layer tables; removes `session_routing` as unnecessary. See `components/state-layer.md` and `components/activities-outbound-delivery.md` for the corresponding schema/activity changes.
