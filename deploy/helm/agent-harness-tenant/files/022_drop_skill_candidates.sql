-- docs/components/skill-subsystem.md REVISION 2026-09-02: the write path is
-- collapsed into one online activity (RecordSkill). The two-step
-- candidate → offline-synthesis flow is gone:
--   - RecordSkillOutcome wrote one skill_candidates row per episode;
--   - SkillSynthesize (debounced) later clustered the queue and
--     created/refined skill_procedures.
-- RecordSkill now match-or-inserts against skill_procedures directly at
-- episode close. No queue, no debounce, no candidates table.

DROP TABLE IF EXISTS skill_candidates;
