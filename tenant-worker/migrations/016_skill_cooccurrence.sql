-- Part of an abandoned learned-procedural-memory design (auto-recorded prose
-- procedures retrieved by similarity). Dropped in full by migration 030; kept
-- here only as the historical record of what this table once was.
CREATE TABLE skill_cooccurrence (
  proc_a       text NOT NULL,
  proc_b       text NOT NULL,
  edge         real NOT NULL,
  last_seen_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (proc_a, proc_b),
  CHECK (proc_a < proc_b)
);
CREATE INDEX skill_cooccurrence_proc_a_idx ON skill_cooccurrence (proc_a);
CREATE INDEX skill_cooccurrence_proc_b_idx ON skill_cooccurrence (proc_b);
