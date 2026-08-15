# Component: Session Consolidation
## (Memory Integration — Base Layer)

> STATUS: IN PROGRESS — base mechanism resolved. This is explicitly a first layer, not the full memory system; deeper design (retrieval, episode schema, cross-session linking) is intentionally deferred.

### Role (one line)
A periodic, per-tenant batch job that finds idle sessions, compacts and summarizes them into durable "episodes," and pushes those episodes to a memory backend — the piece of this architecture that turns raw, ever-accumulating session transcripts into something a future turn (or a human) can actually draw on, without keeping every session's full detail live forever.

### Why this exists (recap)
Raised while resolving subagent filesystem isolation (`components/session-filesystem.md`): sessions are deliberately never force-deleted (see that doc and `future-work.md`'s original framing), which means raw transcripts and file diffs accumulate indefinitely. Left alone, that's just growing storage with no compounding value — the point of this component is to periodically distill that raw accumulation into something durable and useful (memory), decoupled from whether the raw session itself is ever cleaned up. Explicitly framed as **part of the harness's memory integration**, not a housekeeping/cleanup mechanism — compaction is a side effect of the process, not its purpose.

### Resolved: Base Mechanism
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

### Open Questions / To Design (substantial — this is intentionally a first pass)
- Episode schema — what an episode actually contains, its granularity (one per session, one per meaningful sub-arc within a long session, etc.).
- Memory backend — what system episodes are pushed to, and the integration contract (push API, format, auth).
- Retrieval — how a future turn's context-hydration step actually draws on consolidated episodes (or whether that's a separate, later capability entirely).
- Exact idle threshold (days) — not yet numerically decided, only established as "much longer than the coordinator's own TTL."
- Cross-session linking / entity resolution (e.g. recognizing the same ongoing project across multiple sessions) — not addressed at all yet.
- Interaction with session filesystem archival (`components/session-filesystem.md`'s open question: if raw files are ever archived, context-hydration needs to notice and rehydrate) — this component is the natural gate for that, but the archival mechanism itself remains undesigned.
- Failure handling for a single session's summarization step failing mid-batch (skip and retry next run, presumably — not yet decided explicitly).

### Notes Log
- 2026-08-07: Introduced as a scaffold while resolving subagent filesystem isolation — parked in `future-work.md` §3 pending a real design pass.
- 2026-08-07: Promoted to a full component doc. Resolved the base mechanism: daily per-tenant Temporal Schedule, incremental consolidation via a `last_consolidated_turn_seq` watermark on `sessions`, output framed explicitly as "episodes" pushed to an external memory backend. Deliberately kept episode schema, memory backend choice, and retrieval out of scope for this pass — this is the base layer of memory integration, not the full system.
