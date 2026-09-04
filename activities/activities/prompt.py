"""Prompt assembly — request pipeline step 9
(docs/components/request-pipeline/09-prompt-assembly.md).

Turns `lcm.assemble`'s conversation (system prompt + summary DAG + verbatim
window) plus everything the request pipeline staged for the turn — retrieved
skills (step 5), the plan ledger (step 8), discovered tools (step 7), long-term
memory (step 4) — into one ordered, budget-bounded conversation. Runs inside
`ModelCall`, every call; `llm.build_conversation` is a thin call-through kept as
the stable call site.

Section order is most task-specific first, live conversation last (so it
dominates — memory-slot.md's "Resolved: Staleness Is Handled by Placement").
Only `capabilities` and `memory` are ever shed under budget pressure — the
skills and the plan ledger are the most task-critical, and dropping the live
conversation itself is the compression gate's job (context-slot.md), not this
module's.
"""

from __future__ import annotations

import json
import logging
from dataclasses import dataclass

from . import capabilities, ids, lcm, plan

logger = logging.getLogger(__name__)

_SKILLS_HEADER = (
    "Procedures from past successful runs that may fit this task — follow one "
    "where it fits, adapt or ignore it where the situation differs:\n"
)
# Planning-turn only since the 2026-09-04 per-task-resolution revision
# (tool-registry.md, "Resolved: Three-Layer Tool Taxonomy") — a reasoning /
# checkpoint turn gets these same rows as directly-callable schemas instead
# (capabilities.mint_resolved), so this text section no longer renders for it.
_CAPABILITIES_HEADER = (
    "Capabilities available for this task — reference only; the checkpoint "
    "turn that executes each step calls them by name directly:\n"
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

# Shed order when over budget — least task-critical first. `skills` and the plan
# ledger are never in this list: they ARE the task, and there's little point
# running the model without them once they exist.
_SHED_ORDER = ("capabilities", "memory")


@dataclass
class _Section:
    name: str
    text: str
    tokens: int


async def assemble(
    conn, turn_id: str, plan_id: str, system_prompt: str, context_window: int = 0, *, planning: bool = False,
) -> tuple[list[dict], int, list[capabilities.Capability]]:
    """Returns (conversation, context_tokens, resolved_tools).

    `context_tokens` is threaded back through ModelCallOutput to the workflow
    for the compression-gate check (see lcm.assemble's own docstring for why
    it can't be accumulated workflow-side). `resolved_tools` is `ToolDiscover`'s
    staged rows turned into directly-callable `Capability` objects
    (tool-registry.md, "Resolved: Three-Layer Tool Taxonomy & Per-Task
    Resolution") — empty on the **planning turn**, which invokes nothing and
    instead gets those same rows rendered as the "capabilities" text section
    below (a reference catalog, not a callable schema).

    The live conversation is keyed on the session (via turn_id). Memory +
    discovered tools are staged PER TURN (owner_id = turn_id); retrieved skills
    stay plan-scoped (owner_id = plan_id, staged once by the planning turn's
    routing). plan_id is empty for a Lite / conversational turn — the skills and
    plan reads no-op then."""
    session_key = ids.session_key_of(turn_id)
    conversation, context_tokens = await lcm.assemble(conn, session_key, system_prompt)

    sections: list[_Section] = []
    owner = plan_id or turn_id
    staged = await _staged_texts(conn, turn_id, owner)
    resolved_tools = capabilities.mint_resolved(staged["tool_rows"]) if not planning else []
    for name, text in (
        ("skills", staged["skills"]),
        ("plan", await _plan_text(owner)),
        ("capabilities", staged["capabilities"] if planning else None),
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
    # order puts `sections[0]` (skills, if present) first — the order the module
    # docstring describes.
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
    return conversation, context_tokens, resolved_tools


async def _staged_texts(conn, turn_id: str, plan_id: str) -> dict[str, object]:
    """Discovered tools (step 7) + long-term memory (step 4) for THIS turn
    (owner_id = turn_id), and retrieved skills (step 5) for the plan (owner_id =
    plan_id) — one query over turn_retrieval, split by kind. Each `str | None`
    field renders to its section text, or None when nothing was staged;
    `tool_rows` is the raw `(content, metadata)` pairs `capabilities.mint_resolved`
    needs (metadata carries {server, tool, input_schema}, written as jsonb —
    asyncpg hands it back as text, decoded here same as staging.py's own read)."""
    by_kind: dict[str, list[str]] = {}
    tool_rows: list[tuple[str, dict]] = []
    for r in await conn.fetch(
        "SELECT kind, content, metadata FROM turn_retrieval "
        "WHERE (owner_id = $1 AND kind IN ('tool', 'memory')) "
        "   OR (owner_id = $2 AND kind = 'skill') "
        "ORDER BY kind, seq",
        turn_id,
        plan_id,
    ):
        by_kind.setdefault(r["kind"], []).append(r["content"])
        if r["kind"] == "tool":
            tool_rows.append((r["content"], json.loads(r["metadata"]) if r["metadata"] else {}))

    skills = by_kind.get("skill", [])
    tools = by_kind.get("tool", [])
    memory = by_kind.get("memory", [])
    return {
        "tool_rows": tool_rows,
        # skill rows are full rendered procedures — separate with a rule, not a bullet
        "skills": (_SKILLS_HEADER + "\n\n---\n\n".join(skills)) if skills else None,
        "capabilities": (_CAPABILITIES_HEADER + "\n".join(f"- {c}" for c in tools)) if tools else None,
        "memory": (_MEMORY_HEADER + "\n".join(f"- {c}" for c in memory)) if memory else None,
    }


async def _plan_text(plan_id: str) -> str | None:
    checkpoints = await plan.read(plan_id)
    return plan.render_block(checkpoints) or None
