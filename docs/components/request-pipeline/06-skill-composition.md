# Request Pipeline — Step 6: Skill Composition

> STATUS: PHASE 1 BUILT — `activities/activities/retrieval/compose.py`. Reads
> the staged `skill` rows, loads the procedure bodies, and merges them into one
> ordered procedure (a medium-tier model call when there's ≥2 procedures or
> memory/tools to fold in; a straight pass-through of the single procedure's
> render otherwise; degrades to the top render on any failure). Staged as
> `kind='composed'`; `llm.build_conversation` splices it into the prompt.
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
