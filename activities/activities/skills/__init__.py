"""The skill subsystem — a harness-owned procedural memory
(docs/components/skill-subsystem.md, "The Skill Graph").

This package is the subsystem library: the flat `skill_procedures` store,
the embedding helper, the retrieval scoring, and the authored-seed loader.
The pipeline activities that use it live in `activities/activities/retrieval/`
(`SkillDiscover` step 5, `ComposeSkill` step 6) so `RoutingWorkflow` finds
them where it expects.

Built:
  - store.py     — read/write skill_procedures + skill_candidates, EMA updates
  - embedding.py — EMBEDDING_* config → a vector, or None if unconfigured
  - vectors.py   — cosine / normalize / ema_blend (pure)
  - select.py    — pure scoring + greedy selection (phase 1)
  - seed.py      — load seeds/*.json into the store at worker startup (phase 1)
  - record.py    — RecordSkillOutcome activity (phase 2)
  - generalize.py — the medium-tier generalization pass (phase 3)
  - synthesize.py — SkillSynthesize activity (phase 3)

Co-occurrence (phase 4) and the cluster hierarchy (phase 5) are not present yet.
"""

from __future__ import annotations

from . import embedding, generalize, record, seed, select, store, synthesize, vectors

__all__ = [
    "embedding",
    "generalize",
    "record",
    "seed",
    "select",
    "store",
    "synthesize",
    "vectors",
]
