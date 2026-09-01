"""Authored seed procedures — loaded into `skill_procedures` at worker startup.

The seed set is the ongoing quality floor (docs/components/skill-subsystem.md,
"Resolved stances"): synthesis only ever *refines* these, never invents the
common procedures cold. Each `seeds/*.json` file is one procedure:

    { "id": "...", "title": "...", "trigger_text": "...",
      "body": [ { "step_id": "...", "instruction": "...", "tool_ref": "...", "slots": [] } ],
      "preconditions": [...], "done_criteria": [...], "notes": [...] }

`init()` is idempotent — a file whose `trigger_text` is unchanged is skipped
(its embedding and any accumulated confidence are kept). Called once from
`tenant_worker.py`, right after `shell_hub.init()`.
"""

from __future__ import annotations

import json
import logging
from pathlib import Path

from . import embedding, store

logger = logging.getLogger(__name__)

_SEEDS_DIR = Path(__file__).parent / "seeds"


async def init(pool) -> None:
    files = sorted(_SEEDS_DIR.glob("*.json"))
    if not files:
        logger.info("skills.seed: no seed files, skill store starts empty")
        return

    loaded = 0
    for path in files:
        try:
            spec = json.loads(path.read_text())
        except (json.JSONDecodeError, OSError):
            logger.warning("skills.seed: could not read %s, skipping", path.name, exc_info=True)
            continue
        if not spec.get("id") or not spec.get("trigger_text") or not spec.get("body"):
            logger.warning("skills.seed: %s missing id/trigger_text/body, skipping", path.name)
            continue

        existing = await pool.fetchrow(
            "SELECT trigger_text FROM skill_procedures WHERE id = $1 AND provenance = 'authored'",
            spec["id"],
        )
        if existing is not None and existing["trigger_text"] == spec["trigger_text"]:
            loaded += 1
            continue

        vector = await embedding.embed(spec["trigger_text"])
        await store.upsert_authored(pool, spec, vector)
        loaded += 1

    logger.info(
        "skills.seed: %d authored procedure(s) present%s",
        loaded,
        "" if embedding.available() else " (embeddings disabled — not retrievable until configured)",
    )
