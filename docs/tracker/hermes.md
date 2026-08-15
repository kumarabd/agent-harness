# Tracker: Hermes Agent (NousResearch)

> Last verified: 2026-08-07 (against the reasoning captured across `docs/01`–`03` and `docs/components/` during this design pass — not against a fresh read of the upstream Hermes source/repo). Treat entries as `UNVERIFIED` against current upstream unless re-checked.

Hermes is the baseline this entire redesign extends — see `../01-architecture-overall-topology.md` for the "how Hermes works today" summary and `../03-architecture-comparison-vs-hermes.md` for the full dimension-by-dimension rationale behind each divergence below. This file is the running scoreboard; those docs are the "why."

### Legend
- ✅ **Matched** — our design achieves equivalent or better functionality.
- 🔁 **Different by design** — deliberate divergence, not a gap (rationale linked).
- ⏳ **Not yet designed** — gap, needs a design pass.
- ⛔ **Deliberately out of scope** — considered and explicitly deferred/rejected.
- ❓ **Unverified** — claim about Hermes not re-checked against current upstream.

| Area | Hermes today | Our design | Status | Note |
|---|---|---|---|---|
| Agent loop (reason–act–observe) | `AIAgent.run_conversation()`, in-process | Same loop, hosted in a Turn Workflow | ✅ | Loop itself unchanged; only the host changed. See `03-architecture-comparison-vs-hermes.md` §1. |
| Where the turn executes | In-process inside the gateway | Stateless Temporal worker pool | 🔁 | `03` §2. |
| Active-session guard | In-memory, single-process | Session Coordinator + Turn Workflow, workflow-ID single-execution | 🔁 | `02-architecture-temporal-execution.md` §2; `components/session-coordinator.md`. |
| Interrupt / mid-turn follow-up | Queue + interrupt event, checked at loop boundaries | Signal-based; real cooperative cancellation (`WAIT_CANCELLATION_COMPLETED` + heartbeats), not just queue-after | 🔁 | Now a *stronger* guarantee than Hermes' original behavior. `02` §3; `components/activities-outbound-delivery.md`. |
| State store | Single SQLite file, WAL mode, serialized writes | Postgres, real row-level concurrency | 🔁 | `03` §5. |
| Ingestion topology | Single gateway, multi-platform, routes in-process | One gateway type per platform, each independently horizontally scaled (deterministic shard partitioning for connection-based platforms, round-robin for webhook-based) | 🔁 | `components/gateway.md`; `03` §6. |
| Crash recovery | Limited; corruption is a documented failure mode | Temporal durable execution — resumes without replaying side effects | ✅ | `03` §9. |
| Subagents | Existing capability (named in Hermes' isolation primitives) | Recursive child workflow, same workflow type at every tree depth | ✅ | `components/temporal-workflow.md`. |
| Worktree-style file isolation | Existing capability (git worktrees, presumed — see caveat below) | Shared mount + Postgres-backed leases, per-subagent subdirectories; explicitly rejected git-worktree-per-worker | 🔁 | `components/session-filesystem.md`. **Caveat:** our understanding of *how* Hermes does this today is inferred, not verified against source — flag if wrong. |
| Multi-organization / multi-tenant | N/A — not Hermes' design point either | Namespace-per-tenant; shared workflow-worker pool + dedicated activity workers (revised 2026-08-14 from dedicated-everything, via a reference-passing content contract); one self-hosted Postgres instance + PV per tenant | ✅ | `components/multi-tenancy.md`. |
| Pluggable SessionDB (Postgres) | Open RFC upstream, not yet shipped | We build our own Postgres schema rather than wait on it | 🔁 | `01-architecture-overall-topology.md` §6; relationship to the upstream RFC is an open question in `components/state-layer.md`. |
| Session lifecycle / consolidation into memory | ❓ Unknown whether Hermes has anything like this | Periodic per-tenant job: compact/summarize idle sessions into episodes, push to memory backend | ⏳ | `future-work.md` §3. |
| Full-text search over messages | FTS5 (SQLite) | Postgres equivalent — not yet designed | ⏳ | `components/state-layer.md` open questions. |
| Delivery / egress | Separate delivery module, direct send | Persist + deliver activities, dispatched directly to a gateway-embedded Temporal worker via task-queue routing (no message broker) | ✅ | `components/gateway.md`; `03` §8. |

### Known caveats on this comparison
- The "Hermes today" column throughout `docs/01`–`03` was captured from a single earlier read of the Hermes repo/docs, not continuously re-verified. Anything here could be stale if upstream Hermes has moved (e.g., the pluggable-SessionDB RFC may have landed since).
- The worktree-isolation row in particular was flagged during design (`components/session-filesystem.md`) as an *inferred* Hermes capability, not one we've read the source for directly — worth a real verification pass.

### To Do
- Re-verify the whole "Hermes today" baseline against current upstream source (not memory of an earlier pass).
- Check status of the Hermes Postgres SessionDB RFC specifically (also tracked as an open question in `02-architecture-temporal-execution.md` and `components/state-layer.md`).
- Confirm or correct the worktree-isolation assumption.
