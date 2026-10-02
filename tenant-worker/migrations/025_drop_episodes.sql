-- docs/components/episode-lifecycle.md REVISION (Phase 3C, decision B) — fold
-- the `episodes` table away entirely.
--
-- A Deliberate task-run is now a `PlanWorkflow` (workflow id "<plan_id>:plan"),
-- not a row: Temporal's own execution state is the "is a run in progress"
-- source of truth, and `ResolveOpenPlan` (plan_resolve.py) asks it directly.
-- `plan_id` == the anchor / planning turn's id (unchanged from `episode_id`'s
-- meaning), and every turn in the run carries it. The per-run PLAN.md ledger on
-- the tenant PV replaced `turn_plan` (migration 023); `turn_retrieval` is
-- already keyed on a generic `owner_id` (migration 021). Nothing else read
-- `episodes`.

ALTER TABLE turns DROP CONSTRAINT IF EXISTS turns_episode_id_fkey;
DROP INDEX IF EXISTS turns_episode_idx;

ALTER TABLE turns RENAME COLUMN episode_id TO plan_id;
CREATE INDEX turns_plan_idx ON turns (plan_id);

DROP TABLE IF EXISTS episodes;
