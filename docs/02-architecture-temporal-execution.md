# Hermes Agent — Distributed, Scalable Architecture
## Part 2: Temporal Execution Design

### Why Temporal
We model the agent execution layer on Temporal, a durable workflow engine. It's a strong fit because:
- The reason–act–observe loop maps cleanly onto Temporal primitives.
- Temporal gives **durable execution** for free: if a worker dies mid-turn, the workflow resumes without replaying side effects. This directly solves the crash-recovery and state-corruption pain Hermes has today with its single SQLite file.
- Retries, ordering, and single-execution-per-ID guarantees are built in, replacing mechanisms Hermes currently implements in-process.

---

### 1. Mapping the Agent Loop onto Temporal
- **Gateway stays thin.** It persists the incoming message to state (Postgres) and **starts a Temporal workflow**.
- **The workflow = one agent turn.** This is the "rest of Hermes" — the AIAgent turn / conversation loop — lifted out of the gateway into a durable workflow.
- **Each tool call = an activity.** Activities are exactly the retryable, side-effecting units Temporal is built for. The model call is also an activity.
- The reason–act–observe loop runs inside the workflow: reason (model-call activity) → act (tool-call activity) → observe (feed result back) → loop until stop condition.

**Workers** = a pool of stateless Temporal workers running these workflows/activities. They are decoupled from any channel and horizontally scalable — 10 workers or 100 workers, they just chew through the task queue.

---

### 2. Two-Layer Workflow Model: Session Coordinator + Turn Workflows
**Decision:** split "one workflow per session" into two distinct workflow types instead of one long-lived workflow per session:

- **Session Coordinator** (long-lived, one per session): **workflow ID = session key.** A *control-plane* workflow that holds no conversation content — only a pointer to the currently-running turn (if any) and a turn-sequence counter. It is the single durable, addressable target the gateway always talks to via `SignalWithStart`; the gateway never needs to know whether a turn is currently active.
- **Turn Workflow** (short-lived, one per user turn): **workflow ID = `{session_key}:turn:{turn_seq}`**, started as a **child workflow** of the coordinator. Runs the actual reason–act–observe loop for that turn and completes once the turn produces its final response. Bounded, disposable history.

**Why the split:** a single long-lived workflow per session, spanning every turn forever, would accumulate unbounded event history and force continue-as-new gymnastics just to survive a long conversation. Keeping the coordinator itself nearly stateless (it only ever tracks "which turn is active") means it rarely needs continue-as-new, while turn workflows stay small and bounded because each one starts fresh and completes.

**Guard semantics:** the coordinator enforces "only one active turn per session" trivially, in its own single-threaded control flow — it starts a turn child workflow, and while that child is running, any further signal is forwarded into the running child (interrupt) instead of starting a second one. This is still Temporal's per-workflow-ID single-execution guarantee — it now protects the *coordinator's* ID, and the coordinator's own logic protects the turn layer beneath it. This IS the active-session guard — distributed and durable, with no in-memory lock — replacing Hermes' in-process guard, which was the thing that couldn't coordinate across processes.

**Session TTL:** the coordinator self-terminates after **5–15 minutes of idle time** (no active turn and no pending signal — the clock resets on both, and must never fire while a turn is actually running) rather than relying on an external Postgres TTL sweeper. Short on purpose: recreating a coordinator is nearly free (one `SignalWithStart` call, no expensive rehydration — see `components/session-coordinator.md`), while an idle-but-open coordinator has a real ongoing cost at multi-tenant scale (unbounded growth of Temporal's open-workflow/visibility state). Because the gateway always addresses it via `SignalWithStart`, a new message arriving after the coordinator has exited simply recreates it — no race between "marked expired in a DB row" and "message arrives," which a Postgres-TTL-plus-sweeper approach would have.

**Reuse and crash behavior:** `WorkflowIDReusePolicy = AllowDuplicate` on the coordinator's ID — required because the common case (clean TTL exit) and the uncommon case (coordinator crash/failure) both need the next `SignalWithStart` to succeed; `AllowDuplicateFailedOnly` would reject the common clean-exit case, and `RejectDuplicate` would permanently wedge a session after any crash. Separately, `ParentClosePolicy = ABANDON` on the coordinator→turn-child edge means a coordinator crash never tears down an in-flight turn — the turn keeps running, persists, and delivers on its own. The one thing this combination requires downstream: a freshly-started coordinator must not blindly trust a reset "no active turn" pointer — if it tries to start a turn workflow whose ID is still open (an abandoned turn that survived the crash), Temporal rejects the start, and that rejection is the coordinator's signal to attach to the still-running turn instead of assuming none exists.

**Coordinator persists nothing to Postgres on exit.** Turn content is already flushed by each turn's own persist activity as it completes; `turn_seq` is cheaply recomputed on next startup by counting turns already persisted for the session; "no active turn" is just a fresh execution's natural starting state. Session-level bookkeeping like "last active at" should be derived from message timestamps (or kept in sync by the per-turn persist activity) rather than written on the coordinator's rare TTL-exit path.

**Not to be confused with the state layer:** the coordinator's live "which turn is active" pointer is durable via Temporal's own execution history for that workflow, not via a row in our Postgres state layer. Postgres holds the actual conversation *content* (messages, full transcript) — cold, queryable, permanent. The coordinator holds only *control* state, and that state's durability comes from Temporal's own persistence — a separate backing store from our application Postgres. (Both may happen to run on Postgres operationally, but they are logically distinct: Temporal owns its internal schema, we own the sessions/messages schema described in `components/state-layer.md`.)

---

### 3. Interrupt-and-Signal (follow-up messages mid-turn)
This mirrors what Hermes already does conceptually (its guard queues the new message and sets an interrupt event), but modeled durably in Temporal, routed through the two-layer model above.

**Path:** gateway → `SignalWithStart` on the **Session Coordinator** (always the same address, regardless of turn state) → coordinator either:
- forwards the signal into the running **turn child workflow** (a turn is currently in flight), or
- starts a new turn child workflow (no turn currently running).

**Mid-turn arrival (real continuation):** the running turn workflow's signal handler catches the forwarded message, and the workflow **resumes from wherever its own code was** at the next safe loop boundary — genuine Temporal replay-based continuation, since the turn workflow is still an open execution. Fold the new message into context and continue.

**Between-turn arrival (no continuation needed):** if the previous turn workflow already completed, there is nothing to resume — its tree of reasoning steps and tool calls is fully evaluated and closed. The coordinator simply starts a fresh turn child workflow, whose first activity hydrates full context from Postgres. What carries over between turns is *content* (Postgres), not *execution state* — only a still-running turn genuinely resumes mid-execution.

**Chosen behavior:** a genuine interrupt, not queue-after — signaled activities and subagent children are actively cancelled, not left to run to completion untouched.

**Cancellation mechanism — cooperative, not `ABANDON`:**
- On receiving a forwarded signal mid-turn, the workflow requests cancellation of whatever is currently in flight — outstanding tool-call activities in the current reasoning step's parallel fan-out, and/or a running subagent child workflow (cascades via `RequestCancelChildWorkflowExecution`, which recursively cancels whatever that subagent itself has in flight).
- **Activity cancellation type: `WAIT_CANCELLATION_COMPLETED`**, not `ABANDON` and not `TRY_CANCEL`. `ABANDON` was explicitly rejected — it lets the workflow move on instantly, but the underlying activity keeps running unobserved on the worker with an unknown final outcome, which is worse than the latency it saves, especially for side-effecting tools. `WAIT_CANCELLATION_COMPLETED` blocks the workflow until the activity has actually acknowledged cancellation and torn itself down (or hit its own timeout) — the workflow always knows the real final state of a cancelled tool call.
- **Every tool activity must implement cooperative cancellation**: call `heartbeat()` periodically, check for a pending cancellation between chunks of work, and on seeing one, actively tear down what it's doing (kill the subprocess, abort the in-flight HTTP client, close the socket) rather than merely stop heartbeating. This is a per-tool implementation contract, not a Temporal config flag — see `components/activities-outbound-delivery.md`.
- **Honest latency:** this makes interrupt bounded, not instant — actual cancellation latency is *time until the tool's next heartbeat check* plus *its own cleanup time*. That's the intentional trade for never losing track of an activity, and the docs should not imply sub-second responsiveness.
- **Residual limit:** a tool that is one atomic, non-chunkable operation (a single blocking third-party call with no internal checkpoint, e.g. one webhook POST) cannot be interrupted mid-call by any mechanism — cooperative cancellation can only act *between* checkpoints. For such a tool, cancellation takes effect only after that call returns, which is a property of that specific tool, not a gap in this design; it should be flagged per-tool rather than assumed away.

**Safety rule (superseded):** the previous rule — "interruption only happens at loop boundaries, never abort a half-finished tool call" — is now the *fallback* behavior for tools that don't implement cooperative cancellation (or for the atomic-call residual case above), not the general rule. Where a tool does implement heartbeat-based cancellation, it is actively stopped rather than waited out.

---

### 4. The Turn's Internal Shape: Reasoning, Parallel Tool Calls, and Subagent Recursion
Within a single turn workflow, execution is a chain of reasoning steps, each of which can fan out into parallel work:

- **Reasoning step** = one model-call activity. Sequential — each reasoning step depends on the observed results of everything before it.
- **A reasoning step's tool calls are siblings, executed in parallel** (`Promise.all`-style concurrent activity futures within the workflow), not a queue of independent workflows. This mirrors how in-process harnesses like Claude Code already work: one model response can request several tool calls at once, they run concurrently, and their combined results feed the next reasoning step.
- **A tool call is a plain Activity by default.** It only becomes a **Child Workflow** when the "tool" is itself a subagent — something that runs its own nested reason–act–observe loop. Only delegation needs independent workflow identity/history isolation; a leaf tool (shell exec, file read, API call) doesn't.
- **Subagent child workflows are the same turn-workflow shape, invoked recursively.** One workflow *type* implements every level of the tree — main turn or subagent, it's the same code. A subagent's workflow ID nests under its parent's, e.g. `{session_key}:turn:{turn_seq}:sub:{n}`, extended further for deeper nesting — legible in the Temporal UI since the ID mirrors the tree path.
- **No depth or fan-out cap** — decided not to bound subagent recursion depth or parallel tool-call breadth at this layer.
- **Child-workflow parenting:** prefer `ParentClosePolicy = REQUEST_CANCEL` over `TERMINATE` for subagent children, so a parent closing doesn't abruptly kill an in-flight subagent mid-side-effect — consistent with "never abort a half-finished tool call," one level deeper. In practice this rarely fires: interrupts don't close a turn workflow while children are in flight, they signal it to check at its next loop boundary instead.
- **Failure surfacing:** a failed subagent (or tool call) is caught and surfaced to the model as an observation ("subagent failed: ...") rather than an unconditional hard-fail of the whole turn — the model reasons about the failure like it would any other tool error.

---

### 5. Response / Egress Path
Observation captured: inbound and outbound are **separate paths**, not one symmetric round trip. This is already true in Hermes today (inbound: platform adapter → message guard → SessionStore → AIAgent turn; outbound: a separate delivery module). Our design makes the split cleaner and explicit.

**Egress sequence at end of a turn:**
1. The loop produces the assistant's final text response.
2. **Persist activity** — writes the response and updated message history into Postgres (the durable record).
3. **Deliver activity** — sends the response back out through the correct channel. Because it's an activity, failed delivery is retried automatically by Temporal.

---

### 6. Delivery Routing (the tricky part)
**Problem:** the execution worker (Temporal activity) is a **separate process** from the gateway that holds the live Discord/iMessage connection. The worker has the response but does not hold the socket to send it. So the deliver activity can't push directly — it must get the response back to the specific gateway that owns that channel's connection.

**Two options considered:**
- **(A) Outbound queue/channel:** the deliver activity writes to an outbound queue keyed by platform; the owning gateway subscribes and does the actual send. Keeps delivery durable and retryable end to end.
- **(B) Send endpoint:** the gateway exposes a small send endpoint the activity calls directly.

**Decision: Option A (outbound queue).**
- Cleaner, keeps delivery durable and retryable end-to-end.
- **Latency concern raised:** a queue hop could add latency. **Resolution:** a good broker (Redis Streams, NATS) adds only single-digit milliseconds — negligible next to an agent turn that already spent several seconds on model + tool calls. Latency is real but a rounding error here. (Only a heavyweight/badly-tuned broker would bite.)

---

### 7. Full End-to-End Loop (final)
1. **Gateway ingests** a message (owns its channel exclusively) → persists message to Postgres → **`SignalWithStart` on the Session Coordinator** (workflow ID = session key).
2. **Coordinator dispatches** — forwards the signal into the running turn child workflow if one is active, or starts a new turn child workflow (workflow ID = `{session_key}:turn:{turn_seq}`) if idle.
3. **Turn workflow runs the durable turn** — reason (model activity) → act (parallel tool-call activities, or child workflows for subagents) → observe → loop, with interrupt-via-signal at loop boundaries.
4. **Persist** the response + updated history to Postgres.
5. **Deliver** by dropping the response on an **outbound queue** the owning gateway drains and sends on its live platform connection.
6. **Coordinator observes** the turn child workflow's completion, clears its "active turn" pointer, and goes back to waiting — either for the next signal, or an inactivity timeout that ends its own execution (recreated on demand next time).

**Result:** real concurrency across sessions, durability through crashes, correct per-session ordering, and a clean distributed guard with no in-memory locking.

---

### Open Follow-ups (not yet designed in detail)
- Retry policies / timeouts per activity type (model call vs. tool call vs. persist vs. deliver).
- State schema in Postgres (sessions, messages, FTS, routing index).
- How the deliver step addresses the exact owning gateway instance (topic/queue naming per platform + session).
- Current status of the Hermes Postgres SessionDB RFC (what's buildable now vs. coming upstream).
- Whether the 5–15 min coordinator TTL default should vary by platform/tenant.
