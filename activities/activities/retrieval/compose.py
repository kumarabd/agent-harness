"""ComposeSkill — request pipeline step 6
(docs/components/request-pipeline/06-skill-composition.md; design in
docs/components/skill-subsystem.md, "Composition").

Phase 1: read the staged `skill` rows, load the procedure bodies, and — with
the staged `memory` and `tool` rows as context — produce ONE merged, ordered
procedure staged as `kind='composed'`. `build_conversation` (step 9) splices
it into the model's prompt.

A single procedure with nothing to adapt is passed through as its own
render. Otherwise a medium-tier model merges / orders / binds abstract tool
references. Degrades to the top procedure's render if the tier is
unconfigured or the call fails — never fails the turn.
"""

from __future__ import annotations

import logging

from temporalio import activity

from .. import llm_client, model_registry
from ..skills import store
from ..types import ComposeSkillInput, SubsystemResult
from .staging import RetrievalRow, read_rows, write_rows

logger = logging.getLogger(__name__)

_MAX_TOKENS = 900

_SYSTEM_PROMPT = (
    "You are given one or more procedure sketches relevant to the user's current task, "
    "plus (optionally) the user's known preferences and the tools actually available in "
    "this environment. Produce ONE ordered procedure for the agent to follow: merge "
    "overlapping steps, order them sensibly, replace each abstract tool reference (e.g. "
    '"a version-control tool") with a concrete available tool where one is listed, and '
    "fold in the preferences. Keep it tight — numbered steps, a 'Done when:' line, and "
    "any hard 'Note:' cautions. Output only the procedure, no preamble."
)


class ComposeSkillActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ComposeSkill")
    async def __call__(self, input: ComposeSkillInput) -> SubsystemResult:
        rows = await read_rows(self._pool, input.turn_id, ("skill", "memory", "tool"))
        skill_rows = [r for r in rows if r.kind == "skill"]
        if not skill_rows:
            return SubsystemResult(status="empty", count=0)

        proc_ids = [r.metadata.get("procedure_id") for r in skill_rows if r.metadata.get("procedure_id")]
        procedures = await store.procedures_by_ids(self._pool, proc_ids)
        ordered = [procedures[pid] for pid in proc_ids if pid in procedures]
        if not ordered:
            logger.info("ComposeSkill[%s]: staged skill rows resolved to no live procedure", input.turn_id)
            return SubsystemResult(status="empty", count=0)

        memory = [r.content for r in rows if r.kind == "memory"]
        tools = [r.content for r in rows if r.kind == "tool"]

        composed = await self._compose(ordered, memory, tools)
        written = await write_rows(
            self._pool,
            input.turn_id,
            [RetrievalRow(kind="composed", seq=0, content=composed, metadata={"procedure_ids": proc_ids})],
        )
        logger.info(
            "ComposeSkill[%s]: composed from %s (%d chars)", input.turn_id, proc_ids, len(composed)
        )
        return SubsystemResult(status="ok", count=written)

    async def _compose(self, procedures, memory: list[str], tools: list[str]) -> str:
        renders = [p.render() for p in procedures]
        if len(procedures) == 1 and not memory and not tools:
            return renders[0]

        config = model_registry.resolve(*model_registry.default_hint())  # medium tier
        provider = None
        if config.model:
            try:
                provider = llm_client.get_provider(config)
            except RuntimeError as exc:
                logger.info("ComposeSkill: no medium-tier provider (%s) — using top procedure render", exc)
        if provider is None:
            return renders[0]

        parts = ["PROCEDURE SKETCHES:\n" + "\n\n".join(renders)]
        if memory:
            parts.append("USER PREFERENCES / CONTEXT:\n" + "\n".join(f"- {m}" for m in memory))
        if tools:
            parts.append("AVAILABLE TOOLS:\n" + "\n".join(f"- {t}" for t in tools))

        try:
            result = await provider.summarize_text(
                system_prompt=_SYSTEM_PROMPT,
                user_content="\n\n".join(parts),
                model=config.model,
                max_tokens=_MAX_TOKENS,
            )
        except Exception:  # noqa: BLE001 - network/API failure, degrade
            logger.warning("ComposeSkill: merge call failed, using top procedure render", exc_info=True)
            return renders[0]
        return result.content.strip() or renders[0]
