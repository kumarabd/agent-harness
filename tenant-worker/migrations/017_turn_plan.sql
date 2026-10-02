-- The living checkpoint ledger for the since-removed plan-and-execute pipeline
-- phase (see turn-pipeline.md) — the model reported checkpoint advancement via
-- a plan_progress meta-tool, and ModelCall applied each report here. Dropped
-- in full by migration 023; kept here only as the historical record of what
-- this table once was.
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
