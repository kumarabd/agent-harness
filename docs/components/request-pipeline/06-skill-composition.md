# Request Pipeline — Step 6: Skill Composition

> ## REMOVED (2026-09-02)
>
> This step is deleted. Per [`08-planning.md`](08-planning.md)'s plan-and-execute
> revision, there is no separate `ComposeSkill` activity and no `kind='composed'`
> prompt block. The job it did — take the retrieved procedures and turn them into
> one coherent, tool-bound, slot-filled procedure to follow — is now done by the
> **planning turn** inside the `PlanWorkflow`: it reads the `SkillDiscover`
> results (plus memory and discovered tools, like any turn) and drafts a
> checkpoint plan (`propose_plan` → PLAN.md), grounded in those procedures but
> never executing them verbatim. `compose.py` is removed; `CompositionError` and
> the `composePhaseTimeout` go with it.
>
> Kept below for historical context only.
>
> ---
>
> STATUS: PHASE 1 BUILT — `activities/activities/retrieval/compose.py`. Reads
> the staged `skill` rows, loads the procedure bodies, and merges them into one
> ordered procedure (a medium-tier model call when there's ≥2 procedures or
> memory/tools to fold in; a straight pass-through of the single procedure's
> render when there is genuinely nothing to merge/adapt/bind). Staged as
> `kind='composed'`; `llm.build_conversation` splices it into the prompt.
>
> **No fallback.** Except the identity case above, the merge must succeed —
> unconfigured medium tier, provider error, failed call, or unparseable output
> all raise `CompositionError`. There is no "degrade to the top render": a
> composed skill that silently isn't composed misroutes the whole turn. A
> new-episode compose failure propagates through `RoutingWorkflow` and fails
> the turn (`03-routing.md`); a reconcile-mode one records a failed child
> execution and leaves the prior composed row in place.
>
> **Design lives in [`../skill-subsystem.md`](../skill-subsystem.md)** ("The
> Skill Graph"), the Composition section. This file is just the
> pipeline-integration contract.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).

### Pipeline contract

- **Activity:** `ComposeSkill`, tenant-worker. Dispatched by `RoutingWorkflow`
  **only when** `SkillDiscover` (step 5) returned `ok` with `Count > 0`, after
  the memory (step 4) and tool (step 7) fan-out has settled.
- **Input:** `{turn_id}`. Reads the staged `turn_retrieval` rows for
  `kind IN ('skill', 'memory', 'tool')` itself.
- **Does:** order the selected procedures (co-occurrence direction where it
  exists, model otherwise); bind each abstract `tool_ref` to a concrete
  `{server, tool}` from the `kind='tool'` rows; fill slots from `kind='memory'`
  rows or procedure defaults; attach memory adaptation by placement (no
  contradiction reconciliation — see `skill-subsystem.md`); attach provenance +
  confidence as structured metadata.
- **Output:** one composed procedure staged as `turn_retrieval` `kind='composed'`,
  `seq = 0`, with the provenance map in `metadata`.
- **Status:** `ok` (a procedure was staged) | `empty` (nothing to compose) |
  `error` | `timed_out`.

### Consumed by

`llm.build_conversation` (step 9 / `ModelCall`) reads `kind='composed'` alongside
`kind='memory'` and splices it into the prompt. Not wired yet.
