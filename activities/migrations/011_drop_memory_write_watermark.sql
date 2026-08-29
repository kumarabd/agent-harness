-- write_memory.py's second revision (2026-08-29, same day as
-- 009_memory_write_watermark.sql) — WriteMemory is now fully stateless: it
-- sends the session's current active-context merge (every active summary +
-- every never-covered raw message) on every dispatch, not a delta computed
-- against a watermark. Nothing reads or writes this column anymore.
ALTER TABLE sessions DROP COLUMN IF EXISTS memory_write_watermark_turn_seq;
