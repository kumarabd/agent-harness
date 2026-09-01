"""Embedding helper for the skill subsystem.

Reuses the same `EMBEDDING_*` config `shell_hub` already uses
(`EMBEDDING_BASE_URL` / `_API_KEY` / `_MODEL` / `_DIM`) and the same
`zvec.OpenAIDenseEmbedding` client — no new credential, no new dependency.

`embed()` returns `None` whenever the backend is unconfigured or the call
fails: a skill with no `trigger_embedding` is simply not retrievable by
similarity, the same graceful-absence shape as `agent_brain` / `mcp_hub`.
The embedder is built once, lazily, and cached for the process.
"""

from __future__ import annotations

import asyncio
import logging
import os

logger = logging.getLogger(__name__)

_embedder = None
_built = False


def _build():
    base_url = os.environ.get("EMBEDDING_BASE_URL", "")
    if not base_url:
        return None
    import zvec

    return zvec.OpenAIDenseEmbedding(
        model=os.environ.get("EMBEDDING_MODEL", "bge-m3"),
        dimension=int(os.environ.get("EMBEDDING_DIM", "1024")),
        api_key=os.environ.get("EMBEDDING_API_KEY", ""),
        base_url=base_url,
    )


def _get():
    global _embedder, _built
    if not _built:
        _built = True
        try:
            _embedder = _build()
        except Exception:  # noqa: BLE001 - config/import problem, degrade to no embeddings
            logger.warning("skills.embedding: could not build embedder", exc_info=True)
            _embedder = None
        if _embedder is None:
            logger.info("skills.embedding: EMBEDDING_BASE_URL not set — skill similarity retrieval disabled")
    return _embedder


def available() -> bool:
    return _get() is not None


async def embed(text: str) -> list[float] | None:
    """The one-shot embed. zvec's client is synchronous, so it runs in a
    thread. Returns None on any failure — never raises."""
    embedder = _get()
    if embedder is None or not text.strip():
        return None
    try:
        vector = await asyncio.to_thread(embedder.embed, text)
        return [float(x) for x in vector]
    except Exception:  # noqa: BLE001 - network/API failure, degrade
        logger.warning("skills.embedding: embed call failed", exc_info=True)
        return None
