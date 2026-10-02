-- The checkpoint ledger moved to a PLAN.md file on the tenant PV instead of a
-- table — greppable by shell tools and by any delegated Claude Code working
-- the same task, and the exact artifact a human edits at the approval gate.
-- (That whole plan-and-execute mechanism was itself removed shortly after;
-- see turn-pipeline.md.)

DROP TABLE IF EXISTS turn_plan;
