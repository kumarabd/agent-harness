# Component: Session Coordinator

> STATUS: IMPLEMENTED — `workflows/internal/workflow/coordinator.go`. `turn_seq` seeding (line 35, "cheaply recomputed on the next coordinator's startup") is real as of 2026-08-23 via a new `GetMaxTurnSeq` activity; previously stubbed to always start at 0 — see Notes Log.

### Role (one line)
A long-lived, nearly-stateless *control-plane* workflow — one per session, workflow ID = session key — that is the single durable address the gateway always talks to, and whose only job is to know whether a turn is currently active and route accordingly. It is not a data store: it holds a pointer, not conversation content.

### Why this exists (recap)
Originally the design used "session key = workflow ID" directly on the workflow running the agent turn itself. That conflates two different lifetimes: a session can live for days, but a single turn should be bounded and short. A workflow spanning every turn of a long conversation would accumulate unbounded event history. Splitting the two gives each the lifetime it actually needs — see [[temporal-workflow]] (the turn) and `docs/02-architecture-temporal-execution.md` section 2 for the full rationale.

### Responsibilities (from architecture)
- Be the single target of the gateway's `SignalWithStart` calls for a given session — the gateway never needs to know whether a turn is currently active.
- Hold a turn-sequence counter and a pointer to the currently-running turn child workflow, if any.
- On signal: if a turn is active, forward the message into it (interrupt); if not, start a new turn child workflow (workflow ID = `{session_key}:turn:{turn_seq}`).
- Observe the turn child workflow's completion (an awaited child-workflow future) and clear the "active turn" pointer.
- Self-terminate after an inactivity timer with no messages, so it doesn't occupy a running execution indefinitely. Recreated on demand the next time the gateway signals that session key (`WorkflowIDReusePolicy` must allow this).

### Explicitly NOT this component's job
- Does not hold or serve conversation content/history — that's Postgres (`components/state-layer.md`).
- Does not run any model or tool calls itself — that's entirely inside the turn workflow it dispatches to.
- Its own "which turn is active" state is durable via Temporal's own execution history for this workflow, not via a row in our application Postgres. These are two different persistence substrates, even though both may run on Postgres operationally — Temporal owns its internal schema, we own the sessions/messages schema.

### Key Design Decisions (recap)
- Workflow ID = session key; one coordinator per session.
- Turn dispatch via child workflow, not inline execution.
- **Inactivity TTL: 5–15 minutes** of idle time (no active turn AND no pending signal) before the coordinator self-terminates. Chosen because recreating a coordinator is nearly free (one `SignalWithStart` call, no expensive rehydration — see below), while an idle-but-open coordinator has a real ongoing cost at multi-tenant scale (grows Temporal's open-workflow/visibility state without bound). That asymmetry argues for erring short rather than long; may become a per-platform/tenant-tunable default later rather than a single global constant.
  - **Correctness constraint on the clock:** the idle timer must measure time since the coordinator last had *nothing to do* — reset on both an incoming signal and a turn child workflow's completion — not just "time since last message arrived." It must never evaluate/fire while a turn is actively running (a long turn with many tool calls could otherwise span the TTL window even though the session is clearly not idle).
- **`WorkflowIDReusePolicy = AllowDuplicate`** on the coordinator's own ID. Needed because the common case is the *previous* execution closing cleanly (TTL completion) — `AllowDuplicateFailedOnly` would reject exactly that common path (it only permits reuse after a non-clean close), and `RejectDuplicate` would permanently wedge a session after any coordinator exit. `AllowDuplicate` covers both the clean-TTL-exit path and a crashed/failed coordinator equally, so a new message always gets a coordinator to talk to regardless of how the previous one went away.
- **`ParentClosePolicy = ABANDON`** for the coordinator → turn-child edge (distinct from the turn → subagent edge, which uses `REQUEST_CANCEL` — see [[temporal-workflow]]). If the coordinator itself crashes mid-turn, the in-flight turn workflow should keep running to completion, persist, and deliver, unaffected — the coordinator is pure bookkeeping the turn doesn't depend on to keep executing. Losing turn progress because of an unrelated control-plane crash would be a regression, not a feature.
- **Nothing new is persisted to Postgres on exit.** Reasoned out fully below.

### Resolved: what does the coordinator persist to Postgres before exiting?
**Nothing.** Three things that might sound like they need an explicit write already fall out of decisions made elsewhere:
- **Turn content** (messages, responses) is already durably flushed by each turn's own persist activity as that turn completes — nothing is buffered in the coordinator waiting to be written.
- **`turn_seq`** doesn't need to survive the coordinator's shutdown because it's cheaply recomputed on the next coordinator's startup (count of turns already persisted for this session in Postgres) rather than carried in memory across a TTL boundary.
- **"No active turn"** is simply the natural initial state of a freshly-started execution — there's no fact to hand off.

Anything that sounds like session-level bookkeeping ("last active at", "session went dormant") is better derived from data already on the hot path — e.g. `MAX(messages.created_at)` for a given session, or a column kept in sync by the per-turn persist activity — rather than added to the coordinator's rare, TTL-triggered exit path. And any ops/debugging need to see coordinator lifecycle (when did it start/stop, why) should read Temporal's own workflow visibility for that workflow ID rather than mirroring it into Postgres — keeps control-plane facts in one place instead of risking a second copy drifting from the first.

### Resolved: behavior on a failed (not cleanly completed) coordinator
`WorkflowIDReusePolicy = AllowDuplicate` (above) means a `SignalWithStart` after a crashed coordinator succeeds exactly like after a clean TTL exit — a fresh coordinator starts either way. The one real risk is the fresh coordinator's local state (turn_seq, "active turn" pointer) being reset to defaults while an `ABANDON`-ed turn child from the crashed coordinator might *still be running*. This is self-healing rather than a gap: Temporal refuses to start a new workflow execution under an ID that's still open, regardless of reuse policy — so if the new coordinator computes the "next" turn ID and tries to start it, and one with that ID is already running, the start call fails with "already started." The coordinator should treat that failure as ground truth ("a turn actually is still active — attach to it") rather than trusting its own freshly-reset pointer. In other words: the coordinator's in-memory "active turn" pointer is a fast-path cache, and Temporal's own execution status is the fallback source of truth used to reconcile after a crash.

### Open Questions / To Design
- Whether the 5–15 min default should vary by platform/tenant (e.g., a coding-agent session vs. a chat session may have different natural "thinking time" gaps).
- Metrics/observability: how we'd notice a coordinator stuck or leaking (e.g., never hitting its TTL).

### Notes Log
- 2026-08-06: Introduced as a separate component from the turn workflow, to resolve the "workflow lifecycle is undefined" gap — see `docs/02-architecture-temporal-execution.md` section 2.
- 2026-08-06: Closed out TTL duration (5–15 min, idle-since-last-turn-or-signal), `WorkflowIDReusePolicy = AllowDuplicate`, `ParentClosePolicy = ABANDON` for coordinator→turn, and confirmed the coordinator persists nothing to Postgres on exit.
- 2026-08-23: **Fixed a real bug**, found as a side effect of live-verifying `components/context-slot.md`'s CompressContext dispatch: `coordinator.go` had shipped with `turnSeq` hardcoded to `0` on every startup instead of the "recomputed from Postgres" behavior this doc already described as the design (line 35) — so every fresh `CoordinatorWorkflow` execution (i.e. any time the prior one idled out past `idleTTL` and a later message started a new one) reminted `turn:1` again, colliding with turns the session already had. Confirmed directly before the fix: 13 real, separately-sent messages against one session all landed under a single reused `turn_id`. Fixed by adding a new Temporal Activity, `GetMaxTurnSeq` (`activities/activities/get_max_turn_seq.py`, `SELECT MAX(turn_seq) FROM turns WHERE parent_id = $1 AND parent_type = 'session'` — the same query `cmd/starter/main.go` already used client-side to predict this value), called once at the top of `CoordinatorWorkflow` to seed `turnSeq` instead of hardcoding it. The Coordinator can't query Postgres directly without breaking Temporal's determinism boundary, hence the activity rather than an inline query. Verified live: scaled the cluster's `tenant-worker`/`loop-worker` Deployments to 0, ran locally-built binaries against the real port-forwarded Temporal/Postgres, signalled an existing session (`test-compress-1`, which already had a `turn:1` row and no running coordinator) twice with realistic idle gaps — confirmed via direct Postgres query that `turn:2` then `turn:3` were minted contiguously, not `turn:1` again. Cluster Deployments restored to their original replica counts afterward; the fix is in source but the live cluster's tenant-worker/loop-worker images were not rebuilt/redeployed as part of this — they still run the pre-fix code until the next image build.
