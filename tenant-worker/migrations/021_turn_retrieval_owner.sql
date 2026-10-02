-- 2026-09-02 revision: the staged retrieval bundle stops being one-per-episode
-- and runs once per turn instead, so the key column is neither "turn" nor
-- "episode" — it's "whichever unit owns this row." Renamed to say that.
-- (Episodes and the episode-scoped retrieval subsystems this originally
-- distinguished between are themselves long gone — see turn-pipeline.md.)

ALTER TABLE turn_retrieval RENAME COLUMN episode_id TO owner_id;
