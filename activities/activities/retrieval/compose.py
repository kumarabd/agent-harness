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

import json
import logging
import re

from temporalio import activity

from .. import llm_client, model_registry, plan
from ..skills import store
from ..types import ComposeSkillInput, SubsystemResult
from .staging import RetrievalRow, read_rows, write_rows

logger = logging.getLogger(__name__)

_MAX_TOKENS = 1100

_SYSTEM_PROMPT = (
    "You are given one or more procedure sketches relevant to the user's current task, "
    "plus (optionally) the user's known preferences and the tools actually available in "
    "this environment. Produce ONE ordered procedure for the agent to follow: merge "
    "overlapping steps, order them sensibly, replace each abstract tool reference (e.g. "
    '"a version-control tool") with a concrete available tool where one is listed, and '
    "fold in the preferences.\n\n"
    "Output ONLY a JSON object — no prose, no markdown fences — with exactly:\n"
    '{\n'
    '  "procedure": "the procedure as tight numbered steps, then a \'Done when:\' line, '
    "then any hard 'Note:' cautions\",\n"
    '  "checkpoints": [ { "intent": "one line — what this step accomplishes", '
    '"done_when": "the observable condition that closes it" } ]\n'
    "}\n"
    "The checkpoints mirror the numbered steps, in the same order — one per step."
)


class ComposeSkillActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ComposeSkill")
    async def __call__(self, input: ComposeSkillInput) -> SubsystemResult:
        rows = await read_rows(self._pool, input.episode_id, ("skill", "memory", "tool"))
        skill_rows = [r for r in rows if r.kind == "skill"]
        if not skill_rows:
            return SubsystemResult(status="empty", count=0)

        proc_ids = [r.metadata.get("procedure_id") for r in skill_rows if r.metadata.get("procedure_id")]
        procedures = await store.procedures_by_ids(self._pool, proc_ids)
        ordered = [procedures[pid] for pid in proc_ids if pid in procedures]
        if not ordered:
            logger.info("ComposeSkill[%s]: staged skill rows resolved to no live procedure", input.episode_id)
            return SubsystemResult(status="empty", count=0)

        memory = [r.content for r in rows if r.kind == "memory"]
        tools = [r.content for r in rows if r.kind == "tool"]

        composed, checkpoints = await self._compose(ordered, memory, tools)
        written = await write_rows(
            self._pool,
            input.episode_id,
            [RetrievalRow(kind="composed", seq=0, content=composed, metadata={"procedure_ids": proc_ids})],
        )
        # Seed the plan ledger (request-pipeline/08-planning.md). Best-effort —
        # a composed skill with no usable checkpoints just means the loop runs
        # with the prose block but no progress tracking. A reconcile-mode
        # compose regenerates the prose only: the model has been tracking the
        # existing checkpoints via plan_progress, and re-seeding mid-turn would
        # desync those reports.
        seeded = 0
        if checkpoints and not input.reconcile:
            try:
                async with self._pool.acquire() as conn:
                    seeded = await plan.seed(conn, input.episode_id, checkpoints)
            except Exception:  # noqa: BLE001 - never fail compose over plan seeding
                logger.warning("ComposeSkill[%s]: plan seed failed", input.episode_id, exc_info=True)
        logger.info(
            "ComposeSkill[%s]: composed from %s (%d chars, %d checkpoints)",
            input.episode_id, proc_ids, len(composed), seeded,
        )
        return SubsystemResult(status="ok", count=written)

    async def _compose(self, procedures, memory: list[str], tools: list[str]) -> tuple[str, list[dict]]:
        """Returns (procedure prose, checkpoints). Checkpoints degrade to the
        top procedure's own step bodies when the merge call is unavailable."""
        renders = [p.render() for p in procedures]
        fallback_checkpoints = [
            {"intent": s.get("instruction", "").strip(), "done_when": ""}
            for s in procedures[0].body
            if s.get("instruction", "").strip()
        ]

        if len(procedures) == 1 and not memory and not tools:
            return renders[0], fallback_checkpoints

        config = model_registry.resolve(*model_registry.default_hint())  # medium tier
        provider = None
        if config.model:
            try:
                provider = llm_client.get_provider(config)
            except RuntimeError as exc:
                logger.info("ComposeSkill: no medium-tier provider (%s) — using top procedure render", exc)
        if provider is None:
            return renders[0], fallback_checkpoints

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
            return renders[0], fallback_checkpoints

        prose, checkpoints = _parse_compose_output(result.content)
        return (prose or renders[0]), (checkpoints or fallback_checkpoints)


def _parse_compose_output(raw: str) -> tuple[str, list[dict]]:
    """Pull (procedure prose, checkpoints) out of the merge call's JSON. If the
    model returned bare prose instead, keep it as the procedure and derive no
    checkpoints (the caller falls back)."""
    text = (raw or "").strip()
    if text.startswith("```"):
        text = re.sub(r"^```[a-zA-Z0-9]*\n?", "", text)
        text = re.sub(r"\n?```$", "", text).strip()
    obj = None
    for candidate in (text, text[text.find("{") : text.rfind("}") + 1] if "{" in text else ""):
        try:
            parsed = json.loads(candidate)
            if isinstance(parsed, dict):
                obj = parsed
                break
        except (json.JSONDecodeError, ValueError):
            continue
    if obj is None:
        return text, []

    prose = str(obj.get("procedure", "") or "").strip()
    checkpoints = [
        {"intent": str(c.get("intent", "")).strip(), "done_when": str(c.get("done_when", "") or "").strip()}
        for c in obj.get("checkpoints", [])
        if isinstance(c, dict) and str(c.get("intent", "")).strip()
    ]
    return prose, checkpoints
