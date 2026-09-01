-- docs/components/skill-subsystem.md — phase 1 (retrieve + compose over a seed set).
--
-- The flat store: one row per procedure *version*. Retrieval only ever sees
-- rows where valid_to IS NULL.
--
-- trigger_embedding is real[] rather than a pgvector column: the deploy
-- Postgres image has no vector extension, and at seed-set / few-hundred scale
-- a Python cosine over every current row is fine. pgvector + the cluster
-- hierarchy is a later phase (skill-subsystem.md "Cluster hierarchy"). null
-- when the embedding backend was unconfigured at write time — such a row is
-- simply not retrievable by similarity, same graceful-absence shape as the
-- rest of the retrieval phase.
--
-- confidence is the EMA value estimate (skill-subsystem.md "Confidence"),
-- stored, not derived from counts. run_count is evidence volume only. Both are
-- 0 / prior at creation and only move once phase 2 (recording) exists;
-- authored seeds start at confidence 0.7.
CREATE TABLE skill_procedures (
  id                text NOT NULL,                          -- stable across versions
  version           int  NOT NULL DEFAULT 1,
  title             text NOT NULL,
  trigger_text      text NOT NULL,
  trigger_embedding real[],
  body              jsonb NOT NULL,                          -- [{step_id, instruction, tool_ref, slots[]}]
  preconditions     jsonb NOT NULL DEFAULT '[]'::jsonb,
  done_criteria     jsonb NOT NULL DEFAULT '[]'::jsonb,
  notes             jsonb NOT NULL DEFAULT '[]'::jsonb,
  provenance        text NOT NULL CHECK (provenance IN ('learned', 'authored', 'corrected')),
  source_ids        jsonb NOT NULL DEFAULT '[]'::jsonb,      -- member_candidate_ids
  scope             text NOT NULL DEFAULT 'global',          -- global | tenant | project:<x> | user:<x>
  cluster_radius    real,                                    -- per-proc assignment radius from its own trajectory spread; null until synthesis has >=2 to measure
  confidence        real NOT NULL DEFAULT 0.25,
  run_count         int  NOT NULL DEFAULT 0,
  created_at        timestamptz NOT NULL DEFAULT now(),
  updated_at        timestamptz NOT NULL DEFAULT now(),
  last_used_at      timestamptz,
  valid_from        timestamptz NOT NULL DEFAULT now(),
  valid_to          timestamptz,                             -- set when a newer version supersedes this row
  superseded_by     text,                                    -- "{id}:{version}" of the replacement
  PRIMARY KEY (id, version)
);
-- The hot-path read: current rows for the scopes applicable to a session.
CREATE INDEX skill_procedures_current_idx ON skill_procedures (scope) WHERE valid_to IS NULL;
