-- docs/components/skill-subsystem.md — phase 2 (recording).
--
-- One row per completed top-level task turn of moderate/complex complexity
-- (step 2's classification). Written detached at turn end by
-- RecordSkillOutcome, consumed by synthesis (phase 3, not built), then marked
-- done. Never retrievable.
--
-- outcome is the terminal reward: 'success' | 'failure'. A turn corrected
-- mid-task that then succeeded is 'success' with required_correction = true —
-- a weaker success (counts as 0.5 in the confidence EMA).
--
-- transcript is the full trajectory as text: the user turns, the assistant's
-- responses, and a compact list of the tool calls with their outcomes.
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
