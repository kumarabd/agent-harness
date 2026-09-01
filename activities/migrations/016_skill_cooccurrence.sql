-- docs/components/skill-subsystem.md — phase 4 (the co-occurrence graph).
--
-- One undirected edge per pair of procedures that have been composed into the
-- same turn (or the same session). `edge` is an EMA of the pair's joint
-- success (0..1), the retrieval `w_co` weight directly — no separate
-- count-and-decay bookkeeping. RecordSkillOutcome updates it at turn end;
-- SkillDiscover reads it.
--
-- Keyed by procedure `id` (version-agnostic — a pairing survives a re-version).
-- No FK: `skill_procedures.id` isn't unique on its own (it repeats across
-- versions), and an orphan edge is harmless — it never matches in retrieval
-- and is pruned by the forget sweep in update_cooccurrence.
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
