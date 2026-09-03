# Request Pipeline — Step 6: Skill Composition (removed)

This step no longer exists. There is no `ComposeSkill` activity and no
`kind='composed'` prompt block.

The job it did — take the retrieved procedures and turn them into one coherent
procedure to follow — is now the **planning turn**'s inside the `PlanWorkflow`
([`08-planning.md`](08-planning.md)): it reads `SkillDiscover`'s rows (full
rendered procedures, staged under the plan_id) alongside memory and discovered
tools, and drafts a checkpoint plan (`propose_plan` → PLAN.md), grounded in
those procedures but never executing them verbatim.

Removed: `activities/activities/retrieval/compose.py`, `CompositionError`,
`composePhaseTimeout`, `RoutingResult.ComposedSkill`. The pipeline step numbers
skip 6.
