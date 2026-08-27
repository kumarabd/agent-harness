"""ModelCall activity — the reasoning step, and (under the reference-passing
contract) the sole owner of turn content on the reasoning path.

Real design (docs/components/temporal-workflow.md, "Resolved:
Reference-Passing Contract" + "Resolved: Reference/ID Schema"): the workflow
never holds message content, tool names' arguments, or model output text —
only IDs. So this activity:
  1. Looks up a scripted response in `_test_scripted_responses` (test-fixture
     path — see the module docstring on why this table exists at all). If
     none exists, calls a real model provider (llm.py — OpenAI) instead of
     faking success; that seam is the only branch point here.
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
import time

from temporalio import activity
from temporalio.exceptions import ApplicationError

from . import ids, llm, model_registry, permissions
from .types import ModelCallInput, ModelCallOutput, ToolCallRef, Usage

logger = logging.getLogger(__name__)

# docs/components/gateway.md's "Resolved: ModelCall Streaming" — the signal
# TurnWorkflow (turn.go) listens on for chunk-ready notifications. Payload
# is a bare int (the new chunk's seq) — turn_id is already the signaled
# workflow's own ID, no need to repeat it; content never crosses this
# signal at all, matching the reference-passing contract (turn.go holds no
# message content) — the workflow only ever learns "a chunk is ready",
# then reads its actual text from turn_deliveries by ID, same as every
# other activity here.
MODEL_CALL_CHUNK_SIGNAL = "ModelCallChunk"


class ModelCallActivity:
    """Bound-method activity so the Postgres pool, OpenAI client, and
    Temporal client (all created once in tenant_worker.py) are injected per-
    process rather than held as module-global state — the idiomatic way to
    give a Temporal Python activity shared resources without globals.

    temporal_client is used only for the streaming path below (signaling
    the parent TurnWorkflow as chunks become ready) — an activity has no
    other supported way to push data to its own workflow mid-flight; this
    is the documented pattern (an activity using its own client, distinct
    from the worker's own gRPC connection to the server for task polling)."""

    def __init__(self, pool, openai_client, temporal_client):
        self._pool = pool
        self._openai_client = openai_client
        self._temporal_client = temporal_client

    @activity.defn(name="ModelCall")
    async def __call__(self, input: ModelCallInput) -> ModelCallOutput:
        async with self._pool.acquire() as conn:
            fixture = await conn.fetchrow(
                "SELECT content, tool_calls, usage FROM _test_scripted_responses "
                "WHERE turn_id = $1 AND seq = $2",
                input.turn_id,
                input.context_seq,
            )
            if fixture is not None:
                content: str = fixture["content"]
                raw_tool_calls: list[dict] = json.loads(fixture["tool_calls"])
                raw_usage: dict = json.loads(fixture["usage"])
                usage = Usage(
                    input_tokens=raw_usage.get("input_tokens", 0), output_tokens=raw_usage.get("output_tokens", 0)
                )
                # Fixture path never assembles real context (no LCM, no
                # compression-gate concern for a scripted scenario) — 0 is a
                # safe default, same "no real work" treatment already given
                # to the latency histogram below. Same for context_window;
                # turn.go falls back to its static thresholds when it's 0.
                context_tokens = 0
                context_window = 0
                next_hint_modality, next_hint_tier = model_registry.default_hint()
            else:
                session_row = await conn.fetchrow(
                    "SELECT system_prompt, platform FROM sessions WHERE session_key = $1",
                    ids.session_key_of(input.turn_id),
                )
                system_prompt = (session_row["system_prompt"] if session_row else None) or llm.DEFAULT_SYSTEM_PROMPT
                platform = session_row["platform"] if session_row else None
                conversation, context_tokens = await llm.build_conversation(conn, input.turn_id, system_prompt)

                # docs/components/model-registry.md, "Resolved: Selection
                # Mechanism" + "Resolved: Escalate-on-Retry" — the previous
                # step's hint picks the tier (bootstrap default if this is
                # the turn's first call); a Temporal-driven retry of this
                # same activity attempt escalates it by one tier per
                # attempt, capped at expert, regardless of what the hint
                # said — a fast-tier model producing unparseable output is
                # exactly the failure this exists to recover from.
                hint_modality = input.hint_modality or model_registry.default_hint()[0]
                hint_tier = input.hint_tier or model_registry.default_hint()[1]
                attempt = activity.info().attempt
                for _ in range(attempt - 1):
                    hint_tier = model_registry.escalate(hint_tier)
                model_config = model_registry.resolve(hint_modality, hint_tier)
                context_window = model_config.context_window

                # docs/components/budget-guardrails.md, "Resolved: Metrics Export" —
                # real provider round-trip time only; the fixture path above isn't
                # real work and would just add noise to the histogram.
                histogram = activity.metric_meter().create_histogram_float(
                    "model_call_latency_seconds", unit="s"
                )
                started = time.monotonic()
                # docs/components/gateway.md's "Resolved: ModelCall Streaming"
                # — scoped to "single-shot turns only": context_seq == 0 is
                # this turn's first (and, for the common case, only)
                # ModelCall call. Every later iteration (context_seq > 0,
                # meaning an earlier call already had tool calls) uses the
                # exact same unchanged non-streaming path as before this
                # feature existed. Also gated on platform being one
                # turn.go's own streaming-aware path actually handles
                # (2026-08-26: widened from "discord" alone to include
                # "discord-voice" — TurnWorkflow now dispatches
                # VoiceDeliverChunk per chunk the same way it dispatches
                # DiscordDeliverChunk, synthesizing and playing each
                # sentence as it's generated instead of waiting for the
                # whole response) — streaming for any other platform would
                # just be wasted turn_deliveries writes and a signal nobody
                # ever receives.
                if input.context_seq == 0 and platform in ("discord", "discord-voice"):
                    real = await self._call_model_streaming_with_delivery(input.turn_id, conversation, model_config.model)
                else:
                    real = await llm.call_model(self._openai_client, conversation, model_config.model)
                histogram.record(time.monotonic() - started)
                content, raw_tool_calls, usage = real.content, real.raw_tool_calls, real.usage
                next_hint_modality, next_hint_tier = real.next_hint_modality, real.next_hint_tier

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
                    approval_needed, gated_server, gated_tool = (
                        _resolve_gating(tool_name, arguments) if not is_subagent else (False, "", "")
                    )
                    refs.append(
                        ToolCallRef(
                            tool_call_id=tool_call_id,
                            tool_name=tool_name,
                            is_subagent=is_subagent,
                            requires_approval=approval_needed,
                            server=gated_server,
                            tool=gated_tool,
                        )
                    )

            return ModelCallOutput(
                has_tool_calls=len(refs) > 0,
                tool_calls=refs,
                usage=usage,
                context_tokens=context_tokens,
                context_window=context_window,
                next_hint_modality=next_hint_modality,
                next_hint_tier=next_hint_tier,
            )

    async def _call_model_streaming_with_delivery(self, turn_id: str, conversation: list[dict], model: str):
        """Wraps llm.call_model_streaming with this feature's two other real
        pieces (docs/components/gateway.md's "Resolved: ModelCall
        Streaming"): writing each chunk to turn_deliveries and signaling
        TurnWorkflow, and the retry-safety check.

        Retry safety: activity.heartbeat() is called only AFTER a chunk has
        already been written and signaled — i.e. only after it's already
        visible to a user. A retry of this exact activity task (worker
        crash, timeout) checks activity.info().heartbeat_details first: if
        non-empty, an earlier attempt already streamed real output, and the
        underlying LLM call isn't resumable or guaranteed to reproduce the
        same content — silently calling it again risks a *different*
        response overwriting what someone already saw. Fails loudly
        (non-retryable) instead of attempting that: TurnWorkflow treats
        this the same as any other unrecoverable ModelCall failure, not a
        special silently-papered-over case.
        """
        if len(activity.info().heartbeat_details) > 0:
            raise ApplicationError(
                f"ModelCall streaming for turn {turn_id} already emitted visible output in a "
                "prior attempt (heartbeat_details present) — the underlying LLM call can't "
                "resume from that point and isn't guaranteed to reproduce the same content, "
                "so this attempt fails loudly rather than silently regenerating and "
                "re-streaming over what a user may have already seen. See "
                "docs/components/gateway.md's 'Resolved: ModelCall Streaming'.",
                non_retryable=True,
            )

        seq_holder = [0]

        async def on_chunk(cumulative_text: str) -> None:
            seq_holder[0] += 1
            seq = seq_holder[0]
            async with self._pool.acquire() as chunk_conn:
                await chunk_conn.execute(
                    "INSERT INTO turn_deliveries (turn_id, seq, content) VALUES ($1, $2, $3)",
                    turn_id,
                    seq,
                    cumulative_text,
                )
            handle = self._temporal_client.get_workflow_handle(turn_id)
            await handle.signal(MODEL_CALL_CHUNK_SIGNAL, seq)
            # Only after the chunk is durably written AND signaled — see
            # this method's own docstring on why heartbeat ordering here is
            # exactly what makes the retry-safety check above correct.
            activity.heartbeat(seq)

        return await llm.call_model_streaming(self._openai_client, conversation, model, on_chunk)

def _resolve_gating(tool_name: str, arguments: dict) -> tuple[bool, str, str]:
    """docs/components/user-input.md — the {server, tool} identity being
    gated depends on how the model actually invoked things, since
    permissions.requires_approval needs a real {server, tool} pair and
    neither shell_exec nor call_tool's own top-level tool_name IS that pair
    directly. Returns (requires_approval, server, tool) — server/tool are
    only meaningful when requires_approval is True, threaded through to
    ToolCallRef so turn.go can build a human-facing approval prompt without
    ever seeing the call's actual arguments (crosses the reference-passing
    boundary as routing metadata, same category as tool_name itself).

      - shell_exec: server is always "shell" (matching shell_hub.search()'s
        own result shape); tool is deliberately just the first
        whitespace-delimited token of the command string — a known, accepted
        simplification, not real shell parsing (won't catch a compound
        command or one invoked via `sh -c "..."`).
      - call_tool: server/tool are already explicit in its own arguments —
        this is the one case with an exact, unambiguous identity.
      - anything else (memory_search, search_tools, declare_next_step_hint,
        ...): never gateable, these aren't side-effecting.
    """
    if tool_name == "shell_exec":
        command = str(arguments.get("command", "")).strip()
        first_token = command.split(maxsplit=1)[0] if command else ""
        if permissions.requires_approval("shell", first_token):
            return True, "shell", first_token
        return False, "", ""
    if tool_name == "call_tool":
        server = arguments.get("server", "")
        tool = arguments.get("tool", "")
        if permissions.requires_approval(server, tool):
            return True, server, tool
        return False, "", ""
    return False, "", ""
