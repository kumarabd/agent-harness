-- docs/components/request-pipeline/08-planning.md — the living checkpoint ledger.
--
-- Unlike turn_retrieval (migration 013), which the routing phase writes once at
-- turn start and never mutates, turn_plan changes throughout the turn: the model
-- reports checkpoint advancement via the plan_progress meta-tool, and ModelCall
-- applies each report here. So it gets its own table rather than another
-- turn_retrieval kind.
--
-- ComposeSkill (step 6) seeds the rows from the merged procedure's ordered
-- steps. If no skill was composed there is no ledger and the reason-act loop
-- runs exactly as it does today — this is strictly additive, same posture as
-- every other pipeline phase.
--
-- The final state is read by RecordSkillOutcome (skill-subsystem.md,
-- "Recording") and folded into the trajectory handed to synthesis — the
-- checkpoints in their final order, with revised/skipped/added steps marked,
-- ARE the effective procedure the successful run followed.
CREATE TABLE turn_plan (
  turn_id     text NOT NULL REFERENCES turns(turn_id),
  cp_id       text NOT NULL,            -- stable id the model references in plan_progress ("cp1", "cp2", ...)
  checkpoint  int  NOT NULL,            -- ordinal position, 1-based; seeded contiguous, appended steps get MAX+1
  intent      text NOT NULL,            -- one line: what this step accomplishes
  done_when   text NOT NULL DEFAULT '', -- the observable condition that closes it (may be blank for derived checkpoints)
  status      text NOT NULL DEFAULT 'pending'
              CHECK (status IN ('pending', 'active', 'done', 'revised', 'skipped')),
  note        text,                     -- why revised/skipped, or a correction folded in
  updated_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, cp_id)
);
CREATE INDEX turn_plan_order_idx ON turn_plan (turn_id, checkpoint);
