-- Part of an abandoned learned-procedural-memory design (auto-recorded prose
-- procedures retrieved by similarity). Dropped in full by migration 022; kept
-- here only as the historical record of what this table once was.
CREATE TABLE skill_candidates (
  id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  turn_id             text NOT NULL REFERENCES turns(turn_id),
  task_text           text NOT NULL,
  task_embedding      real[],
  transcript          text NOT NULL,
  outcome             text NOT NULL CHECK (outcome IN ('success', 'failure')),
  required_correction boolean NOT NULL DEFAULT false,
  composed_from       jsonb NOT NULL DEFAULT '[]'::jsonb,   -- skill_procedures.id list retrieved into this turn
  created_at          timestamptz NOT NULL DEFAULT now(),
  synthesized_at      timestamptz                            -- null until a synthesis run consumes it
);
CREATE INDEX skill_candidates_unsynthesized_idx ON skill_candidates (created_at) WHERE synthesized_at IS NULL;
