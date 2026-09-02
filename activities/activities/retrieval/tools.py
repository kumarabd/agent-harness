"""ToolDiscover — request pipeline step 7
(docs/components/request-pipeline/07-tool-discovery.md).

Resolves which of this tenant's registered capabilities are relevant to the
task, via the same `discover_tools` primitive the model-facing `search_tools`
tool uses (mcp-hub semantic search + in-process shell-hub, one combined
ranked list). Stages the results to `turn_retrieval` as `kind='tool'` — a
task-scoped tool set for the planner and skill composer, and an implicit
"is this capability connected" answer (an unregistered backend is simply
absent).

Advisory, not restrictive: the reason-act loop still offers the full
always-on tool set and the model can call `search_tools` itself mid-turn.

Failure posture matches `MemoryRetrieve`: not-configured backends degrade to
an empty list inside `discover_tools`; a genuine call failure propagates and
`RoutingWorkflow`'s `RetryPolicy` handles it, then records `error`.
"""

from __future__ import annotations

import logging

from temporalio import activity

from ..metrics import observe_outcome
from ..tools import discover_tools
from ..types import SubsystemResult, ToolDiscoverInput
from .staging import RetrievalRow, write_rows

logger = logging.getLogger(__name__)

_TOP_K = 10
_MAX_DESCRIPTION_CHARS = 300
_SCORE_KEYS = ("score", "similarity", "rrf_score")


def _query(input: ToolDiscoverInput) -> str:
    """The retrieval query, sharpened with any named entity not already in
    it — "is Grafana connected?" wants "Grafana" in the search text even if
    the classifier's phrasing dropped it."""
    query = input.retrieval_query.strip()
    extra = [e.strip() for e in input.entities if e.strip() and e.strip().lower() not in query.lower()]
    return f"{query} {' '.join(extra)}".strip() if extra else query


def _score(result: dict) -> float | None:
    for key in _SCORE_KEYS:
        value = result.get(key)
        if isinstance(value, (int, float)):
            return float(value)
    return None


def _rows(results: list[dict]) -> list[RetrievalRow]:
    seen: set[tuple[str, str]] = set()
    rows: list[RetrievalRow] = []
    for result in results:
        server = str(result.get("server", "")).strip()
        tool = str(result.get("tool", "")).strip()
        if not tool or (server, tool) in seen:
            continue
        seen.add((server, tool))
        description = str(result.get("description", "")).strip()
        content = f"{server}/{tool}" if server else tool
        if description:
            content += f" — {description[:_MAX_DESCRIPTION_CHARS]}"
        rows.append(
            RetrievalRow(
                kind="tool",
                seq=len(rows),
                content=content,
                score=_score(result),
                metadata={"server": server, "tool": tool, "input_schema": result.get("input_schema")},
            )
        )
    return rows


class ToolDiscoverActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ToolDiscover")
    @observe_outcome("tool_discover_total")
    async def __call__(self, input: ToolDiscoverInput) -> SubsystemResult:
        query = _query(input)
        if not query:
            logger.info("ToolDiscover[%s]: empty query — nothing to discover", input.episode_id)
            return SubsystemResult(status="empty", count=0)

        results = await discover_tools(query, _TOP_K)
        rows = _rows(results)
        if not rows:
            logger.info("ToolDiscover[%s]: no tools discovered (query=%r)", input.episode_id, query)
            return SubsystemResult(status="empty", count=0)

        written = await write_rows(self._pool, input.episode_id, rows)
        logger.info("ToolDiscover[%s]: staged %d tool rows (query=%r)", input.episode_id, written, query)
        return SubsystemResult(status="ok", count=written)
