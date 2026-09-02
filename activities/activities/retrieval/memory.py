"""MemoryRetrieve — request pipeline step 4
(docs/components/request-pipeline/04-memory-retrieval.md).

Pulls the user/tenant adaptation layer for the task from agent-brain
(`memory_search` — already RRF-fused server-side across its six retrieval
surfaces), dedups, applies a relevance floor + token budget, and stages the
survivors to `turn_retrieval` as `kind='memory'`. `llm.build_conversation`
reads those rows every ModelCall and renders them into the background block
before the live conversation.

Failure posture:
  - agent-brain not configured / empty query / no results  → `empty`
  - any transient call failure → **raised**, so `RoutingWorkflow`'s
    `RetryPolicy{MaximumAttempts: 3}` retries it; after that it settles as
    `error`. No in-activity retry loop (that was the old, pre-pipeline shape
    when this ran inside ModelCall).
"""

from __future__ import annotations

import logging

from temporalio import activity

from .. import agent_brain, lcm
from ..metrics import observe_outcome
from ..types import MemoryRetrieveInput, SubsystemResult
from .reconcile import reconcile_query
from .staging import RetrievalRow, read_rows, replace_rows, write_rows

logger = logging.getLogger(__name__)

# How many fused results to ask agent-brain for. Numeric-tuning-deferred like
# every other threshold in this project.
_SEARCH_LIMIT = 15
# Drop fused results scoring below this. 0.0 keeps everything — agent-brain's
# own score scale isn't pinned down yet, so the floor is a no-op until it is.
_RELEVANCE_FLOOR = 0.0
# Cap on total staged memory text (lcm.estimate_tokens). The background block
# is enrichment, not the bulk of the prompt.
_TOKEN_BUDGET = 1500

# Fused-result fields that might carry a relevance score, in preference order.
_SCORE_KEYS = ("score", "rrf_score", "fused_score", "rank_score")


def _item_text(result: dict) -> str | None:
    """The most useful single line for one fused result — per source, mirroring
    agent-brain's FusedResult shapes (each source surfaces a different field).
    None when there is nothing renderable."""
    if result.get("statement"):
        return str(result["statement"]).strip()
    if result.get("term") and result.get("definition"):
        return f"{result['term']}: {result['definition']}".strip()
    semantic_fact = (result.get("emu") or {}).get("semantic_fact")
    if semantic_fact:
        parts = (
            semantic_fact.get("subject") or semantic_fact.get("subject_name") or "",
            semantic_fact.get("predicate") or "",
            semantic_fact.get("object_value") or semantic_fact.get("object") or "",
        )
        line = " ".join(p for p in parts if p).strip()
        return line or None
    if result.get("content"):
        return str(result["content"]).strip()
    return None


def _score(result: dict) -> float | None:
    for key in _SCORE_KEYS:
        value = result.get(key)
        if isinstance(value, (int, float)):
            return float(value)
    return None


def _select(results: list[dict]) -> list[RetrievalRow]:
    """Dedup + relevance floor + token budget over agent-brain's already-ranked
    fused results, preserving rank order. Dedup is normalized-text exact match
    — cheap, no embeddings; MMR-style diversity is a later refinement if the
    fused list turns out to be redundant in practice."""
    seen: set[str] = set()
    budget = _TOKEN_BUDGET
    rows: list[RetrievalRow] = []
    for result in results:
        text = _item_text(result)
        if not text:
            continue
        key = " ".join(text.lower().split())
        if key in seen:
            continue
        score = _score(result)
        if score is not None and score < _RELEVANCE_FLOOR:
            continue
        cost = lcm.estimate_tokens(text)
        if cost > budget:
            break
        seen.add(key)
        budget -= cost
        rows.append(
            RetrievalRow(
                kind="memory",
                seq=len(rows),
                content=text,
                score=score,
                metadata={"source": result.get("source", ""), "id": result.get("id", "")},
            )
        )
    return rows


class MemoryRetrieveActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="MemoryRetrieve")
    @observe_outcome("memory_retrieve_total")
    async def __call__(self, input: MemoryRetrieveInput) -> SubsystemResult:
        if input.parent_turn_id and not input.reconcile:
            return await self._inherit(input.episode_id, input.parent_turn_id)

        query = input.retrieval_query.strip()
        if input.reconcile:
            query = await reconcile_query(self._pool, input.turn_id, query)
        if not query:
            logger.info("MemoryRetrieve[%s]: empty query — nothing to retrieve", input.episode_id)
            return SubsystemResult(status="empty", count=0)

        try:
            response = await agent_brain.call_tool("memory_search", {"query": query, "limit": _SEARCH_LIMIT})
        except agent_brain.AgentBrainNotConfiguredError:
            logger.info("MemoryRetrieve[%s]: agent-brain not configured", input.episode_id)
            return SubsystemResult(status="empty", count=0)
        # Any other exception propagates — see module docstring.

        rows = _select(response.get("results", []) or [])
        if not rows:
            logger.info("MemoryRetrieve[%s]: no usable results for query=%r", input.episode_id, query)
            # Reconcile with no hits leaves the original rows in place — an empty
            # result is likelier a noisier query than a signal the old context
            # went stale.
            return SubsystemResult(status="empty", count=0)

        if input.reconcile:
            written = await replace_rows(self._pool, input.episode_id, "memory", rows)
        else:
            written = await write_rows(self._pool, input.episode_id, rows)
        logger.info(
            "MemoryRetrieve[%s]: staged %d memory rows (query=%r, reconcile=%s)",
            input.episode_id, written, query, input.reconcile,
        )
        return SubsystemResult(status="ok", count=written)

    async def _inherit(self, episode_id: str, parent_turn_id: str) -> SubsystemResult:
        """Subagent path (docs/components/episode-lifecycle.md): copy the parent
        episode's already-staged kind='memory' rows rather than re-query
        agent-brain. Memory is about the user's world — stable across a turn
        tree — and the parent's front-loaded snapshot is a consistent
        point-in-time capture."""
        parent_ep = await self._pool.fetchrow(
            "SELECT COALESCE(episode_id, turn_id) AS ep FROM turns WHERE turn_id = $1", parent_turn_id
        )
        parent_episode_id = parent_ep["ep"] if parent_ep else parent_turn_id
        parent_rows = await read_rows(self._pool, parent_episode_id, ("memory",))
        if not parent_rows:
            logger.info(
                "MemoryRetrieve[%s]: parent episode %s staged no memory — nothing to inherit",
                episode_id,
                parent_episode_id,
            )
            return SubsystemResult(status="empty", count=0)
        # re-seq defensively; read_rows already orders by (kind, seq)
        rows = [
            RetrievalRow(kind="memory", seq=i, content=r.content, score=r.score, metadata=r.metadata)
            for i, r in enumerate(parent_rows)
        ]
        written = await write_rows(self._pool, episode_id, rows)
        logger.info(
            "MemoryRetrieve[%s]: inherited %d memory rows from parent episode %s",
            episode_id,
            written,
            parent_episode_id,
        )
        return SubsystemResult(status="ok", count=written)
