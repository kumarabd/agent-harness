# Component: Dreaming
## (Session Consolidation — Offline Memory Integration)

> STATUS: SUPERSEDED — this component's batch-job role is redundant with agent-brain's own real, already-deployed consolidation pipeline. Not deleted (kept as history, same treatment `memory-slot.md` gave its own OKF pivot) — see "Superseded: This Component's Batch-Job Role" below and Notes Log (2026-08-23).

### Role (one line)
A periodic, per-tenant batch job that finds idle sessions, compacts and summarizes them into durable "episodes," and pushes those episodes to a memory backend — the piece of this architecture that turns raw, ever-accumulating session transcripts into something a future turn (or a human) can actually draw on, without keeping every session's full detail live forever.

### Why this exists (recap)
Raised while resolving subagent filesystem isolation (`components/session-filesystem.md`): sessions are deliberately never force-deleted (see that doc and `future-work.md`'s original framing), which means raw transcripts and file diffs accumulate indefinitely. Left alone, that's just growing storage with no compounding value — the point of this component is to periodically distill that raw accumulation into something durable and useful (memory), decoupled from whether the raw session itself is ever cleaned up. Explicitly framed as **part of the harness's memory integration**, not a housekeeping/cleanup mechanism — compaction is a side effect of the process, not its purpose.

### Superseded: This Component's Batch-Job Role
**2026-08-23.** This doc's entire premise — agent-harness owns a periodic, per-tenant batch job that reads raw session data and pushes consolidated episodes to a memory backend — was designed against a hypothetical backend (OKF-on-PV) this project would have had to build all the consolidation logic for. That premise stopped being true on 2026-08-17 when `components/memory-slot.md` adopted agent-brain (hippocampus) as the real backend, and further when it built a real-time `WriteMemory` write-path: every top-level turn already extracts and pushes events to agent-brain immediately (fire-and-forget), not batched by session-idleness at all.

Separately, and more decisively: **agent-brain already runs its own real, already-deployed consolidation pipeline** — checked directly against its source (`/Users/abishekkumar/Documents/agent-brain/workers/temporal-worker/schedules.py`), not assumed. Seven real Temporal Schedules:
```
mining-entities     every 6h
mining-facts        every 6h   (after entities)
mining-rules        every 6h
mining-generalize   daily      — "a consolidation pass, not per-event"
prototypes          every 6h
emu-construction    every 6h   — "community detection -> episode reconstruction -> EMU building"
emu-lifecycle       daily      — "debias/decay/status/merge/split/drift/reindex"
```
`emu-construction` performs literal episode reconstruction from raw events; `emu-lifecycle` is an ongoing consolidation/maintenance pass. This is the job this doc set out to design, already built, already running, on agent-brain's own side — not something agent-harness needs a second copy of.

**What's left, reassigned rather than left dangling:**
- Episode schema/granularity, idle-threshold sizing, batch-failure handling — all moot, there's no batch job left to design them for.
- Interaction with session filesystem archival — was never really this component's concern once it stopped being the active gate; `components/session-filesystem.md` already independently tracks this as its own open question.
- Cross-session linking / entity resolution — the one genuinely still-open item, moved to live solely in `components/memory-slot.md` (not duplicated here) — see that doc's Open Questions and its 2026-08-23 entry for why this is a real gap, not just inherited staleness.

### Resolved: Base Mechanism (historical — describes the superseded design, not current architecture)
- **One job per tenant**, run on a daily cadence (not hours — daily is the base cadence; sub-daily is not needed at this layer). Implemented as a **Temporal Schedule** (native cron), consistent with how periodic work is already modeled elsewhere in this design (e.g. the `delivered_responses`/`ingested_messages` retention discussion considered and rejected scheduling machinery only because those specific tables didn't need it — this job genuinely does).
- **Runs inside the tenant's own namespace and worker fleet** (`components/multi-tenancy.md`) — this is tenant-scoped batch work, not a shared cross-tenant job, consistent with every other per-tenant isolation boundary already established.
- **Scans that tenant's Postgres for sessions idle past a threshold** — same idle-since-last-turn measurement already used for the session coordinator's own TTL (`components/session-coordinator.md`), just a much longer window (days), and **not yet consolidated past their latest turn** (requires a watermark — see schema below).
- **For each candidate session, a summarization step:**
  1. Reads the session's transcript (`messages`, `tool_calls`) and file-directory diffs (via the session filesystem mount, `components/session-filesystem.md`) since the last consolidation checkpoint — incremental, not full-reprocess, once a session has been consolidated before.
  2. Transforms this into one or more **episodes** — the durable, compacted unit this job produces. Exact episode schema (what fields, what granularity, one episode per session vs. per meaningful sub-arc) is explicitly not designed yet — first-layer scope here is establishing that episodes are the output unit and where they go, not their internal shape.
  3. Pushes the episode(s) to the **memory backend** — kept as an external integration point, not something this component owns or designs. (This environment's own memory-manager skill/summarize protocol is a reasonable model to draw from operationally, but the actual backend/schema for *this harness's* memory is future work, not assumed to be the same system.)
- **Runs entirely out-of-band from the live coordinator/turn path** — a batch reader over Postgres and the session filesystem mount, never signals or touches a running workflow. A session actively mid-turn is simply not a candidate (fails the idle-threshold check).
- **Consolidation and any future archival/deletion are separate decisions on separate timelines.** This component's scope is consolidation only. If raw-session cleanup is ever pursued, it would be a distinct, more conservative step gated behind consolidation having already succeeded for that session — not something this job does itself. (Kept as a boundary, not solved here.)

### Key Design Decisions (recap)
- Daily cadence, per-tenant, Temporal Schedule — not a shared global job, not sub-daily.
- Incremental, checkpointed reprocessing (via a watermark), not full-session reprocessing on every run.
- Out-of-band batch job, structurally incapable of interfering with a live turn.
- Compaction is a byproduct of memory extraction, not the goal — this is memory integration, not a cleanup mechanism.
- Deliberately scoped as a *base* layer: episode schema, memory backend choice/integration, and retrieval (how a future turn actually uses these episodes) are all explicitly out of scope for this pass.

### Resolved: Minimal Schema Addition
One watermark column is enough to support incremental consolidation — no new table needed:

```sql
ALTER TABLE sessions ADD COLUMN last_consolidated_turn_seq int;
```
A session is a candidate when idle past the threshold **and** `last_consolidated_turn_seq` is null or less than its latest `turn_seq`. The consolidation job advances this watermark after a successful push to the memory backend, which is what makes the next run's read incremental rather than a full replay.

### Open Questions / To Design (historical — see "Superseded" above; none of these are active anymore)
- Episode schema — moot, agent-brain's own EMU pipeline owns episode shape now.
- Memory backend — resolved: agent-brain, not OKF (see `memory-slot.md`'s own "Superseded: OKF" section).
- Retrieval — owned by `components/memory-slot.md`.
- Exact idle threshold (days) — moot, no batch job needs one.
- Cross-session linking / entity resolution — the one item that outlived this doc's own relevance; tracked solely in `components/memory-slot.md` now.
- Interaction with session filesystem archival — reassigned to `components/session-filesystem.md`, which already tracks it independently.
- Failure handling for a single session's summarization step failing mid-batch — moot, no batch job.

### Notes Log
- 2026-08-23: **Superseded.** In a live conversation the question "isn't this already covered by the memory backend?" prompted checking agent-brain's actual scheduling code directly rather than assuming — confirmed agent-brain runs its own real, already-deployed 7-schedule consolidation pipeline (`mining-entities/facts/rules/generalize`, `prototypes`, `emu-construction`, `emu-lifecycle`) that already does episode reconstruction and ongoing consolidation maintenance, combined with `memory-slot.md`'s real-time `WriteMemory` write-path (every top-level turn pushes events immediately, no batching by session-idleness). This component's entire premise — agent-harness owning a separate per-tenant batch consolidation job — was designed before agent-brain was adopted as the backend and never revisited afterward, the same stale-premise pattern found and fixed on several other docs this session (`session-coordinator.md`, `state-layer.md`, `session-filesystem.md`, `multi-tenancy.md`). Marked superseded rather than deleted; cross-session linking/entity resolution is the one item that outlived this doc's relevance, moved to live solely in `memory-slot.md`.
- 2026-08-07: Introduced as a scaffold while resolving subagent filesystem isolation — parked in `future-work.md` §3 pending a real design pass.
- 2026-08-07: Promoted to a full component doc. Resolved the base mechanism: daily per-tenant Temporal Schedule, incremental consolidation via a `last_consolidated_turn_seq` watermark on `sessions`, output framed explicitly as "episodes" pushed to an external memory backend. Deliberately kept episode schema, memory backend choice, and retrieval out of scope for this pass — this is the base layer of memory integration, not the full system.
- 2026-08-16: Retrieval split out into its own component doc, `components/memory-slot.md` — this doc stays scoped to the producer/consolidation side only.
- 2026-08-17: `components/memory-slot.md` resolved episode schema (format) and memory backend as OKF documents in an OKF bundle on the tenant's existing session-filesystem PV — this job's output target is now concrete, though exact frontmatter fields and directory conventions remain open there.
- 2026-08-17: Renamed `components/session-consolidation.md` → `components/dreaming.md` at the user's direction, "Session Consolidation" kept as the technical subtitle. The name is a deliberate pointer to the human sleep/memory-consolidation analogy — offline, periodic, distilling raw experience into durable memory without the live system being aware it's happening — which the user wants to dig into in depth (how dreaming actually works biologically, and what of that process is or isn't a good fit here) before reworking this doc's own content around it. This pass is the rename only; the base mechanism above is unchanged and not yet rewritten in the new framing.
