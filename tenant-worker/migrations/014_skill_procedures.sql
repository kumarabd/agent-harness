-- Part of an abandoned learned-procedural-memory design (auto-recorded prose
-- procedures retrieved by similarity). Dropped in full by migration 030; kept
-- here only as the historical record of what this table once was.
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
