-- docs/components/request-pipeline/08-planning.md REVISION (Phase 3 slice B):
-- the checkpoint ledger is a PLAN.md file on the tenant PV
-- ($SESSION_ROOT/session/<session_key>/plans/<episode_id>/PLAN.md), not a
-- table — greppable by shell tools and by any delegated Claude Code working
-- the same task, and the exact artifact a human edits at the approval gate.
-- `activities/activities/plan.py` reads/writes the file directly; every caller
-- (ModelCall, ComposeSkill, CompleteEpisode, RecordSkill) is a tenant-worker
-- activity with the PV mounted.

DROP TABLE IF EXISTS turn_plan;
