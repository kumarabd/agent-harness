-- docs/components/request-pipeline/03-routing.md — the staging table for the
-- request pipeline's retrieval phase.
--
-- The retrieval subsystems (MemoryRetrieve / ToolDiscover / SkillDiscover,
-- steps 4/5/7) write their ranked results here; ComposeSkill (step 6) and
-- later the planner (8) / prompt assembly (9) read them. This is the
-- "bulk retrieved content" side of the reference-passing split: the rows'
-- content can be KBs (memory item text, a composed procedure, tool schemas),
-- so it never crosses the shared loop-worker — RoutingWorkflow carries only
-- a turn_id reference plus per-subsystem status.
--
-- One row set per top-level turn, sharing the turn's lifecycle (same
-- retention story as messages / tool_calls). PK is (turn_id, kind, seq) so a
-- Temporal activity retry re-running a subsystem upserts its rows rather
-- than colliding.
--
-- kind = 'composed' is ComposeSkill's output (the skeleton + placed memory,
-- one logical procedure); the other three are raw ranked candidates from
-- their respective discovery subsystems.
CREATE TABLE turn_retrieval (
  turn_id    text NOT NULL REFERENCES turns(turn_id),
  kind       text NOT NULL CHECK (kind IN ('memory', 'tool', 'skill', 'composed')),
  seq        int  NOT NULL,             -- rank within (turn_id, kind)
  content    text NOT NULL,
  score      real,                      -- subsystem-native relevance score, nullable
  metadata   jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, kind, seq)
);
CREATE INDEX turn_retrieval_turn_id_idx ON turn_retrieval (turn_id);
