"""Prompt assembly — request pipeline step 9
(docs/components/request-pipeline/09-prompt-assembly.md).

Turns `lcm.assemble`'s conversation (system prompt + summary DAG + verbatim
window) plus everything the request pipeline staged for the turn — the
composed skill (step 6), the plan ledger (step 8), discovered tools (step 7),
long-term memory (step 4) — into one ordered, budget-bounded conversation.
Runs inside `ModelCall`, every call; `llm.build_conversation` is now a thin
call-through kept as the stable call site.

Section order is most task-specific first, live conversation last (so it
dominates — memory-slot.md's "Resolved: Staleness Is Handled by Placement").
Only `capabilities` and `memory` are ever shed under budget pressure — the
composed skill and plan progress are the most task-critical, and dropping the
live conversation itself is the compression gate's job (context-slot.md), not
this module's.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass

from . import ids, lcm, plan

logger = logging.getLogger(__name__)

_COMPOSED_HEADER = (
    "Suggested procedure for this task, assembled from past successful runs — "
    "follow it where it fits, adapt or ignore it where the situation differs:\n"
)
_CAPABILITIES_HEADER = (
    "These environment tools look relevant to your task — use call_tool to invoke one:\n"
)
_MEMORY_HEADER = (
    "The following is background from prior sessions and long-term memory, "
    "possibly stale — weigh it against what this conversation has already established:\n"
)

# Enrichment (everything below, excluding the live conversation) may consume at
# most this fraction of the model's context window before sections start
# shedding — leaves headroom for the conversation and the response itself.
# 0 / unknown context_window (fixtures, a misconfigured tier) means "no budget
# info" — sections are never shed, same "degrade to unbounded" posture
# model_call.py already gives a zero context_window elsewhere. Numeric-tuning-
# deferred like every other threshold in this project.
_ENRICHMENT_BUDGET_FRACTION = 0.25

# Shed order when over budget — least task-critical first. composed_skill and
# plan_progress are never in this list: they ARE the task, and there's little
# point running the model without them once they exist.
_SHED_ORDER = ("capabilities", "memory")


@dataclass
class _Section:
    name: str
    text: str
    tokens: int


async def assemble(
    conn, turn_id: str, episode_id: str, system_prompt: str, context_window: int = 0
) -> tuple[list[dict], int]:
    """Returns (conversation, context_tokens) — context_tokens is threaded back
    through ModelCallOutput to the workflow for the compression-gate check (see
    lcm.assemble's own docstring for why it can't be accumulated workflow-side).

    The live conversation is keyed on the session (via turn_id); the enrichment
    sections — composed skill, plan ledger, tools, memory — are keyed on
    episode_id (docs/components/episode-lifecycle.md), so a continuation turn
    renders the episode's one bundle. episode_id is empty for a conversational
    fast-path turn — the enrichment reads all no-op then."""
    session_key = ids.session_key_of(turn_id)
    conversation, context_tokens = await lcm.assemble(conn, session_key, system_prompt)

    sections: list[_Section] = []
    ep = episode_id or turn_id
    # composed skill / tools / memory are all turn_retrieval rows for this
    # episode — one query, split by kind. plan progress is its own table.
    staged = await _staged_texts(conn, ep)
    for name, text in (
        ("composed_skill", staged["composed_skill"]),
        ("plan_progress", await _plan_text(conn, ep)),
        ("capabilities", staged["capabilities"]),
        ("memory", staged["memory"]),
    ):
        if text:
            sections.append(_Section(name, text, lcm.estimate_tokens(text)))

    budget = int(context_window * _ENRICHMENT_BUDGET_FRACTION) if context_window > 0 else None
    if budget is not None:
        enrichment_total = sum(s.tokens for s in sections)
        for shed_name in _SHED_ORDER:
            if enrichment_total <= budget:
                break
            for s in list(sections):
                if s.name == shed_name:
                    logger.info(
                        "prompt.assemble[%s]: enrichment %d > budget %d — shedding %r (%d tokens)",
                        turn_id, enrichment_total, budget, s.name, s.tokens,
                    )
                    sections.remove(s)
                    enrichment_total -= s.tokens

    # Each insert(1, ...) pushes the previous one down, so inserting in reverse
    # order puts `sections[0]` (composed_skill, if present) first — the order
    # the module docstring describes.
    for s in reversed(sections):
        conversation.insert(1, {"role": "system", "content": s.text})
        context_tokens += s.tokens

    if sections:
        logger.info(
            "prompt.assemble[%s]: %d enrichment section(s) [%s], %d ctx tokens (budget %s)",
            turn_id,
            len(sections),
            ", ".join(f"{s.name}:{s.tokens}" for s in sections),
            context_tokens,
            budget if budget is not None else "none",
        )
    return conversation, context_tokens


async def _staged_texts(conn, episode_id: str) -> dict[str, str | None]:
    """The composed skill (step 6), discovered tools (step 7), and long-term
    memory (step 4) for this episode — one query over turn_retrieval, split by
    `kind`. Each renders to its section text, or None when nothing was staged
    (or, for composed, when the staged row is blank)."""
    by_kind: dict[str, list[str]] = {}
    for r in await conn.fetch(
        "SELECT kind, content FROM turn_retrieval "
        "WHERE episode_id = $1 AND kind IN ('composed', 'tool', 'memory') "
        "ORDER BY kind, seq",
        episode_id,
    ):
        by_kind.setdefault(r["kind"], []).append(r["content"])

    composed = by_kind.get("composed", [])
    tools = by_kind.get("tool", [])
    memory = by_kind.get("memory", [])
    return {
        "composed_skill": (_COMPOSED_HEADER + composed[0]) if composed and composed[0].strip() else None,
        "capabilities": (_CAPABILITIES_HEADER + "\n".join(f"- {c}" for c in tools)) if tools else None,
        "memory": (_MEMORY_HEADER + "\n".join(f"- {c}" for c in memory)) if memory else None,
    }


async def _plan_text(conn, episode_id: str) -> str | None:
    checkpoints = await plan.read(conn, episode_id)
    return plan.render_block(checkpoints) or None
