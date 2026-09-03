"""The skill subsystem — a harness-owned procedural memory
(docs/components/skill-subsystem.md, "The Skill Graph").

This package is the subsystem library: the flat `skill_procedures` store, the
embedding helper, the retrieval scoring, and the authored-seed loader. The
pipeline activities that use it live in `activities/activities/retrieval/`
(`SkillDiscover` step 5, `ComposeSkill` step 6).

Built:
  - store.py      — read/write skill_procedures, EMA updates, co-occurrence
  - embedding.py  — EMBEDDING_* config → a vector, or None if unconfigured
  - vectors.py    — cosine / normalize / ema_blend (pure)
  - select.py     — pure scoring + greedy selection
  - seed.py       — load seeds/*.json into the store at worker startup
  - generalize.py — the medium-tier generalization pass
  - record.py     — RecordSkill activity (the write path — REVISED 2026-09-02,
                    subsumes the old RecordSkillOutcome + SkillSynthesize chain)
"""

from __future__ import annotations

from . import embedding, generalize, record, seed, select, store, vectors

__all__ = [
    "embedding",
    "generalize",
    "record",
    "seed",
    "select",
    "store",
    "vectors",
]
