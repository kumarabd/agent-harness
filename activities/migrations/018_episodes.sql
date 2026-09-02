-- docs/components/episode-lifecycle.md — the episode as the unit of work.
--
-- An episode is one task from first message to completion. The pre-LLM pipeline
-- (classify -> route -> discover -> compose -> plan seed) runs ONCE when an
-- episode opens; follow-up turns that continue the task attach to the open
-- episode and only refresh (reconcile) its retrieval. RecordSkillOutcome fires
-- ONCE, when the episode closes, over the whole multi-turn trajectory.
--
-- Before this, turn_plan / turn_retrieval were keyed per turn and
-- RecordSkillOutcome fired per turn — an 8-turn teaching conversation produced
-- 5 plans and 4 fragmented `learned:*` procedures. See the doc / the
-- superpowers-eval Notes Log.
--
-- episode_id = the anchor (first) turn's turn_id — no new id scheme, and the
-- episode is one join from any of its turns. Every turn (top-level OR subagent)
-- gets an episodes row: a subagent's episode is its single turn's lifetime,
-- which is why subagents already behaved correctly. The session-scoped
-- "open episode" lookups filter to parent_type='session' via turns.

CREATE TABLE episodes (
  episode_id      text PRIMARY KEY,                          -- = the anchor turn_id
  session_key     text NOT NULL REFERENCES sessions(session_key),
  status          text NOT NULL DEFAULT 'open'
                  CHECK (status IN ('open', 'complete', 'abandoned', 'superseded')),
  intent          text NOT NULL DEFAULT 'task',              -- the anchor turn's classified intent
  complexity      text NOT NULL DEFAULT 'moderate',          -- the anchor turn's classified complexity
  retrieval_query text NOT NULL DEFAULT '',
  task_embedding  real[],                                    -- anchor-message embedding, for the degraded continuation check
  last_stop_reason text NOT NULL DEFAULT '',                  -- the loop stop_reason of the episode's most recent turn — feeds the outcome reward
  opened_at       timestamptz NOT NULL DEFAULT now(),
  closed_at       timestamptz,
  close_reason    text                                       -- plan_complete | superseded | idle | turn_end | abandoned
);
-- One open top-level episode per session at a time (the attach/supersede
-- invariant). Subagent episodes (anchor turn parent_type='turn') are excluded
-- from that invariant — enforced in application code, not here, since the
-- predicate needs a join.
CREATE INDEX episodes_session_open_idx ON episodes (session_key) WHERE status = 'open';

ALTER TABLE turns ADD COLUMN episode_id text REFERENCES episodes(episode_id);
CREATE INDEX turns_episode_idx ON turns (episode_id);

-- turn_plan / turn_retrieval are now episode-scoped, not turn-scoped: the
-- pipeline stages under the anchor turn_id (== episode_id) once, and follow-up
-- turns of the same episode read the same rows. Table names kept (the churn
-- wasn't worth it); only the key column is renamed for clarity.
ALTER TABLE turn_plan RENAME COLUMN turn_id TO episode_id;
ALTER TABLE turn_retrieval RENAME COLUMN turn_id TO episode_id;
-- The FK still points at turns(turn_id) — an episode_id IS the anchor turn's id.
-- PKs and indexes carry the renamed column automatically.
