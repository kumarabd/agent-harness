"""ComposeSkill — request pipeline step 6
(docs/components/request-pipeline/06-skill-composition.md; design in
docs/components/skill-subsystem.md, "Composition").

Phase 1: read the staged `skill` rows, load the procedure bodies, and — with
the staged `memory` and `tool` rows as context — produce ONE merged, ordered
procedure staged as `kind='composed'`. `build_conversation` (step 9) splices
it into the model's prompt.

**No fallback.** A single procedure with nothing to adapt (no other
procedures, no preferences, no tools to bind) is the identity case — it is
returned unchanged, because there is genuinely nothing to compose, not
because anything failed. Every other path requires the medium-tier merge to
succeed: an unconfigured tier, a provider error, a failed call, or output
that doesn't parse all raise `CompositionError`. The activity does not
degrade to "the top procedure's raw render" — a composed skill that silently
isn't composed is worse than a turn that fails loudly and gets fixed.
"""

from __future__ import annotations

import json
import logging
import re

from temporalio import activity

from .. import llm_client, model_registry, plan
from ..metrics import observe_outcome
from ..skills import store
from ..types import ComposeSkillInput, SubsystemResult
from .staging import RetrievalRow, read_rows, write_rows

logger = logging.getLogger(__name__)

_MAX_TOKENS = 1100


class CompositionError(RuntimeError):
    """Raised whenever ComposeSkill cannot produce a merged procedure —
    unconfigured medium tier, provider error, failed call, or unparseable
    output. There is no fallback render; the activity raises, Temporal
    retries the bounded ladder, and an exhausted retry surfaces (RoutingWorkflow
    propagates it; a new-episode compose failure fails the turn)."""


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
    @observe_outcome("compose_skill_total")
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
        # Seed the plan ledger (request-pipeline/08-planning.md). A composed
        # skill with no checkpoints just means the loop runs with the prose
        # block but no progress tracking — that's a real (if lesser) result,
        # not a degradation, so an empty checkpoint list is fine here. Plan
        # seeding that *errors* is not swallowed: it fails the activity like
        # anything else. A reconcile-mode compose regenerates the prose only —
        # the model has been tracking the existing checkpoints via
        # plan_progress and re-seeding mid-episode would desync those reports.
        seeded = 0
        if checkpoints and not input.reconcile:
            async with self._pool.acquire() as conn:
                seeded = await plan.seed(conn, input.episode_id, checkpoints)
        logger.info(
            "ComposeSkill[%s]: composed from %s (%d chars, %d checkpoints)",
            input.episode_id, proc_ids, len(composed), seeded,
        )
        return SubsystemResult(status="ok", count=written)

    async def _compose(self, procedures, memory: list[str], tools: list[str]) -> tuple[str, list[dict]]:
        """Returns (procedure prose, checkpoints). Raises `CompositionError` if
        a merge is needed but can't be done — never returns a degraded render."""
        renders = [p.render() for p in procedures]

        def _merge_result(result: str) -> None:
            activity.metric_meter().with_additional_attributes(
                {"result": result}
            ).create_counter("compose_skill_merge_total").add(1)

        # Identity case: one procedure, nothing to merge / adapt / bind. Its
        # own steps ARE the checkpoints. Not a fallback — there is nothing to
        # compose, and round-tripping a good procedure through the model only
        # risks mangling it.
        if len(procedures) == 1 and not memory and not tools:
            _merge_result("passthrough")
            passthrough_checkpoints = [
                {"intent": s.get("instruction", "").strip(), "done_when": ""}
                for s in procedures[0].body
                if s.get("instruction", "").strip()
            ]
            return renders[0], passthrough_checkpoints

        config = model_registry.resolve(*model_registry.default_hint())  # medium tier
        if not config.model:
            raise CompositionError("medium language tier is not configured — cannot compose")
        try:
            provider = llm_client.get_provider(config)
        except RuntimeError as exc:
            raise CompositionError(f"no provider for medium tier: {exc}") from exc

        parts = ["PROCEDURE SKETCHES:\n" + "\n\n".join(renders)]
        if memory:
            parts.append("USER PREFERENCES / CONTEXT:\n" + "\n".join(f"- {m}" for m in memory))
        if tools:
            parts.append("AVAILABLE TOOLS:\n" + "\n".join(f"- {t}" for t in tools))

        # A network/API failure propagates unchanged — Temporal's retry ladder
        # covers the transient case; an exhausted retry surfaces upstream.
        result = await provider.summarize_text(
            system_prompt=_SYSTEM_PROMPT,
            user_content="\n\n".join(parts),
            model=config.model,
            max_tokens=_MAX_TOKENS,
        )

        prose, checkpoints = _parse_compose_output(result.content)
        _merge_result("merged")
        return prose, checkpoints


def _parse_compose_output(raw: str) -> tuple[str, list[dict]]:
    """Extract (procedure prose, checkpoints) from the merge call's JSON.
    Extraction tolerates markdown fences and surrounding prose; a response
    with no JSON object, or an object with no non-empty `procedure`, raises
    `CompositionError`. Checkpoints may legitimately be empty (the caller
    just skips plan-seeding then)."""
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
        raise CompositionError(f"merge output was not a JSON object: {text[:200]!r}")

    prose = str(obj.get("procedure", "") or "").strip()
    if not prose:
        raise CompositionError(f"merge output has no 'procedure': {text[:200]!r}")
    checkpoints = [
        {"intent": str(c.get("intent", "")).strip(), "done_when": str(c.get("done_when", "") or "").strip()}
        for c in obj.get("checkpoints", [])
        if isinstance(c, dict) and str(c.get("intent", "")).strip()
    ]
    return prose, checkpoints
