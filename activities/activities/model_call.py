"""ModelCall activity — the reasoning step, and (under the reference-passing
contract) the sole owner of turn content on the reasoning path.

Real design (docs/components/temporal-workflow.md, "Resolved:
Reference-Passing Contract" + "Resolved: Reference/ID Schema"): the workflow
never holds message content, tool names' arguments, or model output text —
only IDs. So this activity:
  1. Looks up a scripted response in `_test_scripted_responses` (test-fixture
     path — see the module docstring on why this table exists at all). If
     none exists, this is where a real model-provider call would go; that
     integration is out of scope for this pass (no LLM wiring exists yet in
     this project) — raise a clear error instead of silently faking success.
  2. Reads prior turn history from `messages` to build context — this
     activity *is* the context-hydration step now, not a separate one.
  3. Inserts its own response into `messages`.
  4. Mints tool_call_id (or subagent turn_id) for each tool call and inserts
     the row into `tool_calls`, including `arguments` — this activity is the
     one holding that content, so it's the one that has to write it.
  5. Returns only IDs/names/usage — never arguments, never message content.
"""

from __future__ import annotations

import json
import logging

from temporalio import activity

from . import ids
from .types import ModelCallInput, ModelCallOutput, ToolCallRef, Usage

logger = logging.getLogger(__name__)


class ModelCallActivity:
    """Bound-method activity so the Postgres pool (created once in worker.py)
    is injected per-process rather than held as module-global state — the
    idiomatic way to give a Temporal Python activity a shared resource
    without a global."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ModelCall")
    async def __call__(self, input: ModelCallInput) -> ModelCallOutput:
        async with self._pool.acquire() as conn:
            fixture = await conn.fetchrow(
                "SELECT content, tool_calls, usage FROM _test_scripted_responses "
                "WHERE turn_id = $1 AND seq = $2",
                input.turn_id,
                input.context_seq,
            )
            if fixture is None:
                raise RuntimeError(
                    f"ModelCall[{input.turn_id}:{input.context_seq}]: no test fixture found and no "
                    "real model provider is configured. Real LLM integration is out of scope for "
                    "this implementation pass (docs/components/temporal-workflow.md) — write a "
                    "scripted response to _test_scripted_responses for this (turn_id, seq), or add "
                    "a real provider call here."
                )

            content: str = fixture["content"]
            raw_tool_calls: list[dict] = json.loads(fixture["tool_calls"])
            raw_usage: dict = json.loads(fixture["usage"])
            usage = Usage(input_tokens=raw_usage.get("input_tokens", 0), output_tokens=raw_usage.get("output_tokens", 0))

            logger.info(
                "ModelCall[%s:%d] -> %r (tool_calls=%d)",
                input.turn_id,
                input.context_seq,
                content[:60],
                len(raw_tool_calls),
            )

            async with conn.transaction():
                # messages.seq is a pure ordering/persistence concern,
                # computed here — decoupled from context_seq (a separate,
                # workflow-tracked fixture-lookup index). They only
                # coincidentally start at the same value; InsertMessage's own
                # start-of-turn write already consumed seq=0, so relying on
                # context_seq directly would collide.
                next_seq_row = await conn.fetchrow(
                    "SELECT COALESCE(MAX(seq), -1) + 1 AS next_seq FROM messages WHERE parent_id = $1",
                    input.turn_id,
                )
                message_row = await conn.fetchrow(
                    "INSERT INTO messages (parent_id, role, content, seq) "
                    "VALUES ($1, 'assistant', $2, $3) RETURNING message_id",
                    input.turn_id,
                    content,
                    next_seq_row["next_seq"],
                )
                message_id = message_row["message_id"]

                # n has to be unique across the WHOLE turn, not just this one
                # response — tool_call_id is a flat Postgres primary key, and
                # a turn's loop can call ModelCall many times (each producing
                # its own tool_calls). Restarting n at 1 per response collided
                # across iterations (e.g. every noop_tool call in a
                # single-tool-call-per-step loop minted "{turn_id}:act:1"
                # again) — found via a real UniqueViolationError running the
                # max-iterations scenario. Offset by the count of tool_calls
                # already minted for this turn so numbering is monotonically
                # increasing turn-wide, matching the doc's "{turn_id}:act:{n}"
                # format literally (n = this tool call's position across the
                # whole turn, not within one reasoning step).
                offset_row = await conn.fetchrow(
                    "SELECT count(*) AS n FROM tool_calls WHERE parent_id = $1", input.turn_id
                )
                n_offset = offset_row["n"]

                refs: list[ToolCallRef] = []
                for i, tc in enumerate(raw_tool_calls, start=1):
                    n = n_offset + i
                    tool_name = tc["name"]
                    is_subagent = bool(tc.get("is_subagent", False))
                    arguments = tc.get("arguments", {})

                    tool_call_id = (
                        ids.subagent_turn_id(input.turn_id, n)
                        if is_subagent
                        else ids.activity_id(input.turn_id, n)
                    )

                    # status left at its 'pending' default — ToolCall (or the
                    # subagent child workflow's own completion) is what
                    # transitions it to ok/error/cancelled. See the schema
                    # migration's note on why 'pending' exists.
                    await conn.execute(
                        "INSERT INTO tool_calls "
                        "(tool_call_id, parent_id, message_id, tool_name, arguments, is_subagent) "
                        "VALUES ($1, $2, $3, $4, $5, $6)",
                        tool_call_id,
                        input.turn_id,
                        message_id,
                        tool_name,
                        json.dumps(arguments),
                        is_subagent,
                    )
                    refs.append(ToolCallRef(tool_call_id=tool_call_id, tool_name=tool_name, is_subagent=is_subagent))

            return ModelCallOutput(has_tool_calls=len(refs) > 0, tool_calls=refs, usage=usage)
