"""Intention activities — docs/components/proactivity.md.

`FireIntention` SignalWithStarts the session coordinator's `Wake` handler when
an `IntentionWorkflow`'s trigger fires — the same entry point the gateway uses
for a user message, so everything downstream is an ordinary turn.

`CheckCondition` runs a poll-kind intention's probe and judges its predicate.
v1 is a stub (always not-fired): the probe-run + predicate-judge path lands with
the genesis daily-review work. Until then, `condition` / `state` / `event`
intentions poll but never fire — `time` / `deadline` / `inactivity` work fully.
"""

from __future__ import annotations

import json
import logging
import os

from temporalio import activity
from temporalio.common import WorkflowIDConflictPolicy

from . import llm_client, mcp_hub, model_registry
from .types import CheckConditionInput, CheckConditionResult, FireIntentionInput

logger = logging.getLogger(__name__)

_COORDINATOR_WORKFLOW = "CoordinatorWorkflow"
_WAKE_SIGNAL = "Wake"

_JUDGE_TIER = "fast"
_JUDGE_MAX_TOKENS = 200
_JUDGE_SYSTEM_PROMPT = (
    "You judge whether a watch condition is met, given the latest result of a probe. "
    'Reply with ONLY a JSON object: {"fired": <true|false>, "note": "<one short line>"}. '
    "Be conservative — fire only when the condition is clearly met."
)


class FireIntentionActivity:
    def __init__(self, pool, temporal_client):
        self._pool = pool  # unused today; kept for symmetry with the other activities
        self._client = temporal_client
        self._task_queue = os.environ.get("TEMPORAL_TASK_QUEUE", "agent-loop")

    @activity.defn(name="FireIntention")
    async def __call__(self, input: FireIntentionInput) -> None:
        # SignalWithStart: reuse the running coordinator if there is one, else
        # start it headless (empty ConnectionID → the proactive turn's output
        # routes to Postgres, surfaced on next open — proactivity.md "Delivery").
        await self._client.start_workflow(
            _COORDINATOR_WORKFLOW,
            {"session_key": input.session_key},
            id=input.session_key,
            task_queue=self._task_queue,
            id_conflict_policy=WorkflowIDConflictPolicy.USE_EXISTING,
            start_signal=_WAKE_SIGNAL,
            start_signal_args=[
                {
                    "intention_id": input.intention_id,
                    "objective": input.objective,
                    "why": input.why,
                }
            ],
        )
        logger.info(
            "FireIntention[%s]: woke coordinator %s", input.intention_id, input.session_key
        )


class CheckConditionActivity:
    """Run a poll-kind intention's probe and judge its predicate.

    Probe: `probe.tool` is a "server/tool" invoked through mcp-hub's own
    `call_tool` (the same path the model's `call_tool` tool uses). Predicate:
    a fast-tier LLM judges the natural-language condition against the result.

    A transient probe/LLM failure propagates so the IntentionWorkflow's retry
    ladder can recover it; a persistent one fails the intention visibly (no
    silent "assume not fired" for a broken probe). A probe that runs but whose
    result doesn't meet the predicate simply returns fired=False.
    """

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="CheckCondition")
    async def __call__(self, input: CheckConditionInput) -> CheckConditionResult:
        probe = input.probe
        if "/" not in probe.tool:
            return CheckConditionResult(fired=False, note=f"probe.tool {probe.tool!r} is not 'server/tool'")
        server, tool = probe.tool.split("/", 1)

        result = await mcp_hub.call_tool(
            "call_tool", {"server": server, "tool": tool, "arguments": probe.args or {}}
        )
        result_text = json.dumps(result)[:4000]

        config = model_registry.resolve("language", _JUDGE_TIER)
        if not config.model:
            raise RuntimeError(f"{_JUDGE_TIER} language tier not configured — cannot judge intention predicate")
        provider = llm_client.get_provider(config)
        judged = await provider.summarize_text(
            system_prompt=_JUDGE_SYSTEM_PROMPT,
            user_content=f"Condition: {probe.predicate}\n\nLatest probe result:\n{result_text}",
            model=config.model,
            max_tokens=_JUDGE_MAX_TOKENS,
        )

        fired, note = _parse_judgement(judged.content)
        logger.info(
            "CheckCondition[%s]: %s/%s → fired=%s (%s)", input.intention_id, server, tool, fired, note
        )
        return CheckConditionResult(fired=fired, note=note)


def _parse_judgement(raw: str) -> tuple[bool, str]:
    start, end = raw.find("{"), raw.rfind("}")
    if start != -1 and end > start:
        try:
            obj = json.loads(raw[start : end + 1])
            return bool(obj.get("fired")), str(obj.get("note", ""))[:200]
        except (ValueError, TypeError):
            pass
    return False, "could not parse predicate judgement"
