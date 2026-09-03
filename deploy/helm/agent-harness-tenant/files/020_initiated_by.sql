-- docs/components/proactivity.md — turn/episode provenance.
--
-- A proactive turn is the ordinary reason-act turn, started by a trigger the
-- agent set for itself (an IntentionWorkflow firing) rather than by a user
-- message. `initiated_by` is the only thing that distinguishes it downstream:
--   'user'      — a real inbound message (the default; every turn today)
--   'intn:<id>' — an IntentionWorkflow fired and woke the coordinator
--   'plan'      — a checkpoint / planning turn under a PlanWorkflow (08-planning)
--
-- The seed message a proactive turn starts from is still written role='user',
-- seq=0 (ClassifyRequest requires that shape) — this column carries the "not
-- actually the user" signal instead.

ALTER TABLE turns    ADD COLUMN initiated_by text NOT NULL DEFAULT 'user';
ALTER TABLE episodes ADD COLUMN initiated_by text NOT NULL DEFAULT 'user';
