-- docs/components/episode-lifecycle.md REVISION + request-pipeline REVISION
-- (2026-09-02): the staged retrieval bundle stops being one-per-episode.
--
-- MemoryRetrieve and ToolDiscover now run once PER TURN (fresh — they duplicated
-- the lcm -> WriteMemory -> agent-brain -> MemoryRetrieve loop when frozen at
-- episode open). Only SkillDiscover / ComposeSkill stay episode-scoped (they
-- seed the plan once). So the key column is neither "turn" nor "episode" — it's
-- "whichever unit owns this row": the current turn for memory/tool, the episode
-- anchor turn for skill/composed. Rename it to say that.
--
-- (Migration 018 renamed this column turn_id -> episode_id; this supersedes that
-- for turn_retrieval. turn_plan keeps episode_id — the plan ledger is genuinely
-- episode-scoped. The whole table goes away in the plan-and-execute phase.)

ALTER TABLE turn_retrieval RENAME COLUMN episode_id TO owner_id;
