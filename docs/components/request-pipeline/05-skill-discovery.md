# Request Pipeline — Step 5: Skill Discovery

> STATUS: BUILT — `activities/activities/retrieval/skills.py` +
> `activities/activities/skills/` (store, embedding, select, seed). Embeds the
> `retrieval_query`, flat-cosines against the current procedures for the
> session's scopes, greedy budget-bounded selection (`skills.select`, no
> co-occurrence / recency term yet), stages `kind='skill'` rows under the
> plan_id. 4 authored seed procedures. Cluster hierarchy not built (flat scan).
>
> **Design lives in [`../skill-subsystem.md`](../skill-subsystem.md)** ("The
> Skill Graph"). This file is the pipeline-integration contract. Also supersedes
> the reverted mcp-hub document-store design in [`../skills.md`](../skills.md).
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).

### Pipeline contract

- **Activity:** `SkillDiscover`, tenant-worker, dispatched in the
  `RoutingWorkflow` fan-out on a **planning turn** only (`RoutingWorkflowInput.PlanID`
  set). Checkpoint / continuation / Lite turns skip it.
- **Input:** `{plan_id, retrieval_query}` from step 2's `TaskRepresentation`.
- **Does:** the retrieval algorithm in `skill-subsystem.md` — embed the query,
  flat cosine over the session's procedures, score by
  `sim + co-occurrence + confidence + recency − diversity`, greedy
  budget-bounded selection.
- **Output:** staged `turn_retrieval` rows keyed `owner_id = plan_id`,
  `kind='skill'` — one per selected procedure, `content` = the full rendered
  procedure, `score` = selection score, `metadata` =
  `{procedure_id, version, provenance, confidence}`.
- **Consumers:** `prompt.assemble` renders the rows into the planning turn's
  prompt; `RecordSkill` reads the `procedure_id`s back to attribute the run's
  reward.
- **Status:** `ok` (≥ 1 selected) | `empty` (no store, nothing over the floor,
  empty query) | `error` (embedding outage — raised, `RoutingWorkflow` retry
  handles it) | `timed_out`.

### Complementary vs. alternative — resolved

The earlier open question ("RRF ranks S1, S2, S3 but doesn't say S1+S2 compose
while S3 is an alternative") is answered by the **co-occurrence graph**
(`skill-subsystem.md` §8): the `w_co` term in scoring boosts procedures that
succeeded *together* in real runs, so a coherent bundle emerges from usage
rather than from an authored hierarchy or a separate LLM selection pass.
