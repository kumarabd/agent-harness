"""The episode — the unit of work (docs/components/episode-lifecycle.md).

An episode is one task from first message to completion. The pre-LLM pipeline
runs ONCE when an episode opens; follow-up turns that continue the task attach
to the open episode and only reconcile-refresh its retrieval. RecordSkillOutcome
fires ONCE, when the episode closes, over the whole multi-turn trajectory.

`episode_id` == the anchor (first) turn's `turn_id`. Every turn — top-level or
subagent — gets an `episodes` row; the session-scoped open/attach invariant
only applies to top-level episodes (anchor turn `parent_type = 'session'`).

This module owns the `episodes` table. Helpers take an open connection; the
activity class (`EpisodeActivities`) wraps them for the workflow layer and is
the one place that needs the embedding backend.
"""

from __future__ import annotations

import logging

from temporalio import activity

from . import ids, plan
from .skills import embedding
from .skills.vectors import cosine
from .types import (
    CloseSessionEpisodesInput,
    CloseSessionEpisodesResult,
    CompleteEpisodeInput,
    CompleteEpisodeResult,
    OpenEpisodeInput,
    OpenEpisodeResult,
)

logger = logging.getLogger(__name__)

# Degraded-path continuation threshold — used only when the classifier didn't
# give a confident `continues_prior` (confidence < 0.5). Biased toward
# continue: a false "new" re-fragments the episode, a false "continue" only
# adds a stale composed block the model can ignore. Numeric-tuning-deferred.
_CONT_FLOOR = 0.55
_RECORD_COMPLEXITIES = {"moderate", "complex"}


async def anchor_text(conn, turn_id: str) -> str:
    row = await conn.fetchrow(
        "SELECT content FROM messages WHERE parent_id = $1 AND seq = 0 AND role = 'user'",
        turn_id,
    )
    return (row["content"] if row and row["content"] else "").strip()


async def _open_top_level(conn, session_key: str):
    """The session's currently-open top-level episode, or None. Subagent
    episodes (anchor turn parent_type='turn') are excluded."""
    return await conn.fetchrow(
        "SELECT e.episode_id, e.task_embedding FROM episodes e "
        "JOIN turns t ON t.turn_id = e.episode_id "
        "WHERE e.session_key = $1 AND e.status = 'open' AND t.parent_type = 'session' "
        "ORDER BY e.opened_at DESC LIMIT 1",
        session_key,
    )


async def _insert(conn, episode_id: str, session_key: str, task, task_embedding) -> None:
    await conn.execute(
        "INSERT INTO episodes (episode_id, session_key, status, intent, complexity, "
        "retrieval_query, task_embedding) VALUES ($1, $2, 'open', $3, $4, $5, $6) "
        "ON CONFLICT (episode_id) DO NOTHING",
        episode_id,
        session_key,
        task.intent or "task",
        task.complexity or "moderate",
        (task.retrieval_query or "")[:2000],
        task_embedding,
    )
    await conn.execute("UPDATE turns SET episode_id = $2 WHERE turn_id = $1", episode_id, episode_id)


async def _close(conn, episode_id: str, status: str, reason: str) -> None:
    await conn.execute(
        "UPDATE episodes SET status = $2, close_reason = $3, closed_at = now() "
        "WHERE episode_id = $1 AND status = 'open'",
        episode_id,
        status,
        reason,
    )


def _should_continue(task, open_embedding, anchor_embedding) -> bool:
    """Continue the open episode, or supersede it? Trust a confident classifier
    verdict; fall back to embedding similarity (biased toward continue) when the
    classifier degraded."""
    if task.confidence >= 0.5:
        return bool(task.continues_prior)
    if not open_embedding or not anchor_embedding:
        return True
    return cosine([float(x) for x in open_embedding], anchor_embedding) >= _CONT_FLOOR


async def open_or_attach(conn, turn_id: str, task, anchor_embedding) -> OpenEpisodeResult:
    """`anchor_embedding` is the embedding of this turn's seed message (computed
    outside the transaction by the caller) — stored as the new episode's
    task_embedding and reused for the degraded continuation check."""
    session_key = ids.session_key_of(turn_id)
    turn_row = await conn.fetchrow("SELECT parent_type FROM turns WHERE turn_id = $1", turn_id)
    parent_type = turn_row["parent_type"] if turn_row else "session"

    # A subagent's episode is its single turn — no session open/attach concept.
    if parent_type == "turn":
        await _insert(conn, turn_id, session_key, task, anchor_embedding)
        return OpenEpisodeResult(episode_id=turn_id, attached=False, superseded_episode_id="")

    open_ep = await _open_top_level(conn, session_key)
    if open_ep is not None:
        if _should_continue(task, open_ep["task_embedding"], anchor_embedding):
            await conn.execute("UPDATE turns SET episode_id = $2 WHERE turn_id = $1", turn_id, open_ep["episode_id"])
            logger.info("episode: turn %s ATTACHED to open episode %s", turn_id, open_ep["episode_id"])
            return OpenEpisodeResult(episode_id=open_ep["episode_id"], attached=True, superseded_episode_id="")
        await _close(conn, open_ep["episode_id"], "superseded", "superseded")
        superseded = open_ep["episode_id"]
    else:
        superseded = ""

    await _insert(conn, turn_id, session_key, task, anchor_embedding)
    logger.info("episode: turn %s OPENED new episode (superseded=%r)", turn_id, superseded)
    return OpenEpisodeResult(episode_id=turn_id, attached=False, superseded_episode_id=superseded)


async def complete_if_plan_done(conn, episode_id: str, stop_reason: str) -> bool:
    await conn.execute(
        "UPDATE episodes SET last_stop_reason = $2 WHERE episode_id = $1",
        episode_id,
        stop_reason or "",
    )
    checkpoints = await plan.read(conn, episode_id)
    if not plan.all_terminal(checkpoints):
        return False
    await _close(conn, episode_id, "complete", "plan_complete")
    logger.info("episode: %s COMPLETE (plan all-terminal)", episode_id)
    return True


async def close_session_open(conn, session_key: str) -> list[str]:
    rows = await conn.fetch(
        "UPDATE episodes SET status = 'complete', close_reason = 'idle', closed_at = now() "
        "WHERE session_key = $1 AND status = 'open' "
        "  AND episode_id IN (SELECT turn_id FROM turns WHERE parent_type = 'session') "
        "RETURNING episode_id",
        session_key,
    )
    return [r["episode_id"] for r in rows]


async def close_subagent(conn, episode_id: str, stop_reason: str) -> None:
    await conn.execute(
        "UPDATE episodes SET last_stop_reason = $2 WHERE episode_id = $1",
        episode_id,
        stop_reason or "",
    )
    await _close(conn, episode_id, "complete", "turn_end")


class EpisodeActivities:
    """Bound-method activities — Postgres pool injected per-process, same
    pattern as ModelCallActivity / ClassifyRequestActivity."""

    def __init__(self, pool):
        self._pool = pool

    async def _embed(self, text: str):
        if not text:
            return None
        try:
            return await embedding.embed(text)
        except Exception:  # noqa: BLE001 - embeddings are best-effort everywhere in this codebase
            logger.warning("episode: embedding failed", exc_info=True)
            return None

    @activity.defn(name="OpenEpisode")
    async def open_episode(self, inp: OpenEpisodeInput) -> OpenEpisodeResult:
        async with self._pool.acquire() as conn:
            text = await anchor_text(conn, inp.turn_id)
        emb = await self._embed(text)  # outside the transaction — it's a network call
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                return await open_or_attach(conn, inp.turn_id, inp.task, emb)

    @activity.defn(name="CompleteEpisode")
    async def complete_episode(self, inp: CompleteEpisodeInput) -> CompleteEpisodeResult:
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                completed = await complete_if_plan_done(conn, inp.episode_id, inp.stop_reason)
        return CompleteEpisodeResult(completed=completed)

    @activity.defn(name="CloseSubagentEpisode")
    async def close_subagent_episode(self, inp: CompleteEpisodeInput) -> CompleteEpisodeResult:
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                await close_subagent(conn, inp.episode_id, inp.stop_reason)
        return CompleteEpisodeResult(completed=True)

    @activity.defn(name="CloseSessionEpisodes")
    async def close_session_episodes(self, inp: CloseSessionEpisodesInput) -> CloseSessionEpisodesResult:
        async with self._pool.acquire() as conn:
            async with conn.transaction():
                episode_ids = await close_session_open(conn, inp.session_key)
        return CloseSessionEpisodesResult(episode_ids=episode_ids)
