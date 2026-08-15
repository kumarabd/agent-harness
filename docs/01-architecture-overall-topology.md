# Hermes Agent — Distributed, Scalable Architecture
## Part 1: Overall Topology & Design Decisions

### Goal
Take the Hermes Agent harness (NousResearch) and re-architect it into a distributed, horizontally scalable, multi-organization system that can handle bursts of messages arriving from many sources at once — processing them in parallel across sessions, concurrently, and with correct ordering where messages within a session are dependent.

> **Current scope note (updated 2026-08-07):** the core execution design (Parts 1–2 of this doc set) was originally built out for a single user before multi-tenancy was addressed, and most of it reads that way still — but multi-tenancy is no longer deferred. Isolation strategy (Temporal namespace-per-tenant with dedicated worker fleets, a Postgres database per tenant, a PersistentVolume per tenant namespace) is resolved — see `components/multi-tenancy.md`. What's still open is narrower: dynamic per-tenant fleet provisioning, and confirming session-key tenant-scoping is fully subsumed by namespace boundaries.

---

### 1. The Core Agent Loop (unchanged, kept from Hermes)
Every agent, Hermes included, runs a reason–act–observe loop:
- **Reason**: the model looks at current state and decides the next step.
- **Act / Execute**: it performs the action (a tool call, a shell command, a query). "Act" and "execute" are the same thing.
- **Observe**: it ingests the result of that action back into context to inform the next round of reasoning.
- The loop also needs a **stopping condition** (how it decides it's done) and role alternation between user/assistant turns.

This is formally close to the **ReAct loop** (Reasoning + Acting). Note: this is NOT the same as an "event loop" (the Node.js runtime concept) — that's a different thing.

Coding harnesses built on this same principle include Codex, Claude Code, OpenCode, OpenClaw (spelled c-l-a-w), and Hermes Agent.

---

### 2. How Hermes Works Today (baseline)
Hermes Agent is from NousResearch. Its core is the **AIAgent** class (in run_agent.py) — a *synchronous* orchestration engine that owns model selection, budget, callbacks, platform context, prompt construction, tool execution, retries, fallback, compression, and persistence.

**Inbound message path today:**
1. Platform adapter receives the raw event (Discord, iMessage, Slack, etc.) and normalizes it into a MessageEvent.
2. Message guard / active-session guard: if the agent is already running for this session, the new message is **queued and an interrupt event is set**. Slash commands like /approve, /deny, /stop bypass the guard.
3. Session key is resolved (format today: agent:main:{platform}:{chat_type}:{chat_id}).
4. Authorization check.
5. SessionStore (state) is updated.
6. One AIAgent turn runs — run_conversation(): append user message, build/reuse cached system prompt, check preflight compression (>50% context), build API messages, make interruptible model call, parse response (if tool_calls → execute, append results, loop; if text → persist, return).

**Outbound path today:** a separate delivery module sends the response back to the platform. So inbound and outbound are already two distinct, connected paths — not one symmetric round trip.

**State today:** a single shared SQLite file (~/.hermes/state.db) in WAL mode. Multiple processes (gateway + CLI + worktree agents) share it. WAL gives concurrent reads but **serialized writes**. Hermes fights write contention with a short 1s timeout, jittered retries (20–150ms, up to 15 retries), BEGIN IMMEDIATE transactions, and periodic WAL checkpoints (every 50 writes) to avoid the "convoy effect."

---

### 3. The Scaling Problem
- The AIAgent turn is **single-threaded per session** and runs **in-process inside the gateway**.
- The **active-session guard lives in a single process's memory**, so it can't coordinate across processes.
- State is **one SQLite file with serialized writes** — under heavy production load this causes contention and even corruption. There is an open RFC in the Hermes repo to make the SessionDB pluggable (Postgres, MySQL) precisely because of this.

**Conclusion:** Hermes gives you per-session concurrency, NOT true multi-tenant horizontal scale out of the box. We extend it — keep the excellent agent loop and tool system, but lift execution out into workers and swap the state layer.

---

### 4. Terminology Clarified
- **Gateway** = the front door / ingestion + routing layer (receives raw events, normalizes to MessageEvent, resolves session key, checks auth, routes slash commands).
- **Conversation manager** = SessionStore + the AIAgent turn together (owns turn history and context).
- These are distinct; the gateway feeds the conversation manager.

---

### 5. Chosen Topology: One Gateway Type Per Platform
**Decision:** deploy a separate gateway *type* per messaging platform (`DiscordGateway`, `WhatsAppGateway`, ...), each internally handling many users/channels/tenants and independently horizontally scaled — not one process per individual channel/chat.

**Why this was chosen:**
- Each gateway type owns its platform's ingestion exclusively, and (for connection-based platforms) each replica owns a disjoint partition of that platform's traffic by construction — so a given conversation only ever lives behind one coordinated path, without a distributed lock.
- Scales horizontally by adding replicas within a platform type, not only by adding new platform types.

> **Superseded reasoning (kept for history, not current):** an earlier version of this decision rejected round-robin replicas specifically because "two messages from the same conversation could land on different replicas, reviving the in-memory guard collision." That reasoning no longer applies — the in-memory active-session guard it refers to was replaced by the Temporal session coordinator (`components/session-coordinator.md`), which uses `SignalWithStart` and is safe to call from any replica regardless of routing. The real remaining constraint on replica routing is **not** guard collision — it's the physical one below (a live socket only exists in one process).

**Resolved replica strategy — this is genuinely platform-dependent, not one rule:**
- **Connection-based platforms** (Discord, Slack Socket Mode): hold a live, stateful socket, so replicas must mirror the platform's own deterministic partitioning (e.g. Discord shards by guild ID — `guild_id % num_shards`) rather than round-robin. This is a physical constraint (a socket lives in one process's memory), not a coordination problem to solve with a lock. Direct messages typically all land on shard 0 regardless — a known platform caveat, not something we can route around.
- **Webhook-based platforms** (WhatsApp, Telegram, Slack Events API): no persistent connection, no partitioning concept — plain round-robin replicas behind a load balancer are fully safe.
- Full design, including deployment model (StatefulSet vs. Deployment) and shard-assignment mechanism (static config, not a dynamic lease), is in `components/gateway.md`.

**Identity caveat (still holds):** if the same human could reach us on two channels and we treated that as one logical session, the collision problem sneaks back. In our model each channel is its own session space → clean.

---

### 6. Required Shared State Layer
**Decision:** replace SQLite with **Postgres** (a real networked datastore).

**Why:**
- Solves the serialized-write wall: many gateways and many execution workers get real row-level concurrency instead of a single-writer file lock.
- All gateways and all workers talk to one shared state layer so any worker can load session context regardless of which gateway ingested the message.

**Important warning captured:** do NOT simply split the gateway out while still sharing the same SQLite file between two processes — that is exactly the multi-process/one-state.db/WAL setup described in the corruption RFC. The networked datastore is what makes the split safe.

**Next-layer problems Postgres surfaces (to design for):**
1. The active-session guard is still in-process — needs a distributed mechanism (addressed by the Temporal design in Part 2).
2. Session affinity and delivery routing (addressed in Part 2).

---

### 7. High-Level Data Flow (target)
- **Ingest path:** gateway receives message → dedup check → durably submits it into Temporal (`SignalWithStart` on the session coordinator) → the message body is persisted to Postgres downstream, from that durable signal payload, not written separately by the gateway itself (see `components/gateway.md`).
- **Execution:** a pool of stateless execution workers runs the actual reason–act–observe loop (this is the "rest of Hermes" — the AIAgent turn), decoupled from any channel and horizontally scalable.
- **Egress path:** worker persists the response to state → a deliver activity is dispatched directly to the gateway worker that owns the target platform/shard via Temporal's own task-queue routing (not a separate message broker), which sends it out on the live platform connection.

(Detailed execution design, ordering, interrupts, and delivery routing are in Part 2: Temporal Execution Design. Full gateway design is in `components/gateway.md`.)
