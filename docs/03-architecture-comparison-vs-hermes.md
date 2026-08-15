# Hermes Agent — Distributed, Scalable Architecture
## Part 3: Our Design vs. Hermes Today — Comparison & Rationale

This doc compares each design decision against how Hermes Agent works today, and records *why* we chose what we chose.

---

### Dimension-by-Dimension Comparison

#### 1. Agent Loop (reason–act–observe)
- **Hermes today:** reason–act–observe loop inside AIAgent.run_conversation(). Solid; this is the part we keep.
- **Our design:** identical loop, but lifted out of the gateway process into a durable Temporal workflow.
- **Why:** the loop itself is good. The problem was never the loop — it was *where* it runs and *how* state/guards are coordinated. So we keep the loop, change its host.

#### 2. Where the turn runs
- **Hermes today:** in-process, inside the gateway. Synchronous orchestration engine.
- **Our design:** in a pool of stateless Temporal workers, decoupled from any channel.
- **Why:** in-process execution ties throughput to the gateway and can't scale independently. Stateless workers scale horizontally — add workers to chew through the queue.

#### 3. Active-session guard (one turn per conversation)
- **Hermes today:** in-memory guard in a single process — if the agent is running for a session, new messages are queued and an interrupt event is set. Cannot coordinate across processes.
- **Our design:** two-layer workflow model — a long-lived **Session Coordinator** (workflow ID = session key) that holds only a pointer to the currently-active **Turn Workflow** (workflow ID = `{session_key}:turn:{turn_seq}`, a child workflow). The coordinator forwards new messages into the running turn or starts a new one, enforcing single-active-turn in its own control flow, backed by Temporal's per-ID single-execution guarantee.
- **Why:** the in-memory guard is the single biggest blocker to multi-process scale. Splitting a stable, long-lived coordinator from disposable, bounded turn workflows gives the same guard semantics without any shared in-memory lock, while avoiding unbounded history growth on a single session-spanning workflow.

#### 4. Interrupt / follow-up messages mid-turn
- **Hermes today:** queues the new message, sets an interrupt event, checks it at loop boundaries.
- **Our design:** new message = `SignalWithStart` on the session coordinator, which forwards it as a signal into the running turn workflow; the turn workflow's signal handler interrupts at a safe loop boundary and folds the message into context. If no turn is running, the coordinator starts a fresh turn workflow instead — that case isn't a continuation, it's a new bounded execution that hydrates context from Postgres. Signals are durable and ordered.
- **Why:** same conceptual behavior, but durable and lossless across crashes. Chosen behavior is interrupt-and-accept, not just queue-after. Distinguishing "mid-turn" (real workflow continuation) from "between-turn" (fresh workflow + DB reload) keeps the model honest about what's actually being resumed.

#### 5. State store
- **Hermes today:** single SQLite file (state.db), WAL mode. Concurrent reads, **serialized writes**. Heavy mitigation (1s timeout, jittered retries, BEGIN IMMEDIATE, WAL checkpoints) to fight the "convoy effect." Known corruption under heavy multi-process load (open Postgres-pluggability RFC in the repo).
- **Our design:** Postgres (networked datastore) with real row-level concurrency.
- **Why:** the serialized-write wall and corruption risk are fundamental to a single shared file. Postgres removes the wall and lets many gateways + workers write concurrently. NOTE: simply splitting processes while still sharing the SQLite file is the *exact* failure mode in the corruption RFC — the networked DB is what makes the split safe.

#### 6. Ingestion topology
- **Hermes today:** gateway receives events from many platforms, routes them, runs the turn in-process. Single-gateway-centric.
- **Our design:** one gateway per source (a gateway per Discord server/channel, per iMessage chat, etc.), each owning its channel exclusively.
- **Why:** exclusive channel ownership means a conversation only ever lives on one gateway *by construction* — no two gateways can receive the same session's messages, so the cross-process guard collision simply cannot occur. No distributed lock needed at the ingestion layer. Simplest design that still scales by adding sources.

#### 7. Load balancing / replicas
- **Hermes today:** N/A at this granularity.
- **Our design:** explicitly NOT round-robin load balancing per channel. Rely on the platform's own sharding (e.g., Discord shards by guild ID → server affinity for free; DMs land on shard 0). No need to run multiple replicas for a single channel because a single conversation never generates enough volume.
- **Why:** round-robin across replicas would revive the guard collision (two messages of one convo hitting two replicas). Per-source ownership + platform sharding avoids it without sticky-routing machinery.

#### 8. Egress / delivery
- **Hermes today:** a separate delivery module sends the response back to the platform (inbound and outbound already distinct paths).
- **Our design:** persist activity (write to Postgres) then deliver activity that drops the response on an **outbound queue** keyed by platform; the owning gateway drains it and sends on its live connection.
- **Why:** the worker doesn't hold the platform socket, so it must hand the response back to the gateway that does. The queue keeps delivery durable and retryable end-to-end. Latency cost (single-digit ms) is negligible against multi-second turns.

#### 9. Crash recovery
- **Hermes today:** limited — a crash mid-turn risks partial state; corruption is a documented failure mode.
- **Our design:** Temporal durable execution — a worker dying mid-turn resumes the workflow without replaying side effects.
- **Why:** durability is a first-class Temporal guarantee and directly retires Hermes' biggest reliability gap.

---

### Summary: What We Keep vs. Change
- **Keep from Hermes:** the reason–act–observe agent loop, the tool system, the notion of a session key, the interrupt-at-loop-boundary discipline, the inbound/outbound split.
- **Change:** lift the turn into stateless Temporal workers; replace the in-memory guard with workflow-ID single-execution; replace SQLite with Postgres; adopt one-gateway-per-source; make delivery a durable queue-backed activity.

### Net Effect
- Real parallelism **across** sessions, correct ordering **within** a session.
- Distributed active-session guard with no in-memory lock.
- Durable, crash-safe turns.
- Horizontal scale by adding sources (ingestion) and workers (execution) independently.

### Honest Caveat
Hermes does NOT provide this out of the box today. It offers per-session concurrency, isolation primitives (profiles, per-session state, subagents), and is *inching* toward pluggable Postgres via an open RFC and async-facade fixes — but the queue + stateless worker pool + networked DB + Temporal-style durability is an **extension** we build on top of Hermes, keeping its strengths (agent loop, tools).
