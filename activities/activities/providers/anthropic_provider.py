"""Anthropic Messages API provider — docs/components/model-registry.md's
"anthropic" provider.

The two APIs are cousins but not identical, and the translation is worth
naming plainly rather than sprinkled across the code:

1. **System prompt is a separate field**, not a message with role="system".
   Anthropic's Messages API takes `system` alongside `messages`, and
   messages must alternate user/assistant starting with user.

2. **Assistant tool calls are content blocks**, not a separate
   `tool_calls` array. An assistant message that invokes tools has
   `content=[{type:"text",...}, {type:"tool_use", id, name, input}, ...]`
   instead of OpenAI's `{content, tool_calls: [{id, function:{name,arguments}}]}`.

3. **Tool results are user-message content blocks**, not messages with
   role="tool". OpenAI's role="tool" message becomes a user message with
   `content=[{type:"tool_result", tool_use_id, content}]`.

4. **Tool schemas** are functionally identical but shaped one layer
   flatter: `{name, description, input_schema}` vs. OpenAI's
   `{type:"function", function:{name, description, parameters}}`.

5. **Streaming events** are a discriminated union (`content_block_delta`
   with `type: text_delta` or `type: input_json_delta`, `message_delta`
   carrying usage, etc.) rather than OpenAI's flat delta shape. Same
   on_chunk sentence-boundary contract for the caller.

The `declare_next_step_hint` meta-tool works exactly the same on both
sides — the model calls it as a normal tool; we strip it out and read
the hint fields from its arguments.
"""

from __future__ import annotations

import json
import logging

from anthropic import AsyncAnthropic

from .. import model_registry, sentence_segmenter
from ..types import Usage
from .base import Provider, SimpleTextResult

logger = logging.getLogger(__name__)

_NEXT_STEP_HINT_TOOL_NAME = "declare_next_step_hint"
# docs/components/temporal-workflow.md's recursion-termination guard —
# is_subagent has to be derived from which tool the model actually called,
# not hardcoded False.
_SPAWN_SUBAGENT_TOOL_NAME = "spawn_subagent"


def _openai_tools_to_anthropic(tools: list[dict]) -> list[dict]:
    """Unwrap OpenAI's {type:"function", function:{...}} envelope into
    Anthropic's flat {name, description, input_schema}."""
    out = []
    for t in tools:
        f = t.get("function", {})
        out.append(
            {
                "name": f.get("name", ""),
                "description": f.get("description", ""),
                "input_schema": f.get("parameters", {"type": "object", "properties": {}}),
            }
        )
    return out


def _openai_conversation_to_anthropic(conversation: list[dict]) -> tuple[str, list[dict]]:
    """Returns (system_prompt, messages). Pulls the system message out
    of `messages` (Anthropic takes it separately), converts assistant
    tool_calls into tool_use content blocks, and converts role="tool"
    messages into user tool_result content blocks — with adjacent
    tool_results coalesced into the same user message, since Anthropic
    requires strict user/assistant alternation.
    """
    system_parts: list[str] = []
    out: list[dict] = []
    for msg in conversation:
        role = msg.get("role")
        if role == "system":
            content = msg.get("content") or ""
            if content:
                system_parts.append(content)
            continue

        if role == "assistant":
            blocks: list[dict] = []
            content = msg.get("content")
            if content:
                blocks.append({"type": "text", "text": content})
            for tc in msg.get("tool_calls") or []:
                # tc shape (from lcm.py): {id, type:"function", function:{name, arguments}}
                fn = tc.get("function", {})
                # arguments is stored as a JSON string in Postgres per
                # tool_calls.arguments's own type; Anthropic wants the
                # parsed object.
                try:
                    input_obj = json.loads(fn.get("arguments") or "{}")
                except (json.JSONDecodeError, TypeError):
                    input_obj = {}
                blocks.append(
                    {"type": "tool_use", "id": tc.get("id", ""), "name": fn.get("name", ""), "input": input_obj}
                )
            if not blocks:
                # An assistant message with neither text nor tool_calls
                # can't be sent to Anthropic — skip rather than send an
                # empty content array (the API rejects it).
                continue
            out.append({"role": "assistant", "content": blocks})
            continue

        if role == "tool":
            # A tool result. Coalesce into the preceding user message if
            # that message was itself already tool-result-only (adjacent
            # tool_results in an OpenAI-shaped conversation become one
            # Anthropic user turn); otherwise start a new user turn.
            tc_id = msg.get("tool_call_id", "")
            content = msg.get("content") or ""
            tool_result_block = {"type": "tool_result", "tool_use_id": tc_id, "content": content}
            if out and out[-1]["role"] == "user" and isinstance(out[-1].get("content"), list) and all(
                isinstance(b, dict) and b.get("type") == "tool_result" for b in out[-1]["content"]
            ):
                out[-1]["content"].append(tool_result_block)
            else:
                out.append({"role": "user", "content": [tool_result_block]})
            continue

        if role == "user":
            content = msg.get("content") or ""
            out.append({"role": "user", "content": content})
            continue

        # Unknown role — skip rather than corrupt the conversation.
        logger.warning("AnthropicProvider: skipping message with unrecognized role %r", role)

    return "\n\n".join(system_parts), out


class AnthropicProvider(Provider):
    def __init__(self, base_url: str, api_key: str):
        # base_url is optional for Anthropic — the SDK has a working
        # default (api.anthropic.com). Passing an empty string would
        # override that with an invalid URL, so only pass base_url when
        # it's actually non-empty.
        kwargs: dict = {"api_key": api_key}
        if base_url:
            kwargs["base_url"] = base_url
        self._client = AsyncAnthropic(**kwargs)

    async def call_model(self, conversation, model, max_tokens, tools):
        from .. import llm as _llm

        system, messages = _openai_conversation_to_anthropic(conversation)
        response = await self._client.messages.create(
            model=model,
            max_tokens=max_tokens,
            system=system,
            messages=messages,
            tools=_openai_tools_to_anthropic(tools),
        )

        content_parts: list[str] = []
        raw_tool_calls: list[dict] = []
        next_hint_modality, next_hint_tier = model_registry.default_hint()

        for block in response.content:
            btype = getattr(block, "type", None)
            if btype == "text":
                content_parts.append(block.text or "")
            elif btype == "tool_use":
                if block.name == _NEXT_STEP_HINT_TOOL_NAME:
                    # Anthropic returns input as an already-parsed object,
                    # not a JSON string, so no json.loads needed.
                    hint_args = block.input or {}
                    if isinstance(hint_args, dict):
                        next_hint_modality = hint_args.get("modality", next_hint_modality)
                        next_hint_tier = hint_args.get("tier", next_hint_tier)
                    else:
                        logger.warning(
                            "AnthropicProvider.call_model: non-dict %s input, using default hint",
                            _NEXT_STEP_HINT_TOOL_NAME,
                        )
                    continue
                # Internal shape stores arguments as an already-parsed dict
                # (json.loads-ed on the OpenAI side); match that shape.
                raw_tool_calls.append(
                    {
                        "name": block.name,
                        "arguments": block.input or {},
                        "is_subagent": block.name == _SPAWN_SUBAGENT_TOOL_NAME,
                    }
                )

        usage_obj = response.usage
        usage = Usage(
            input_tokens=getattr(usage_obj, "input_tokens", 0) or 0,
            output_tokens=getattr(usage_obj, "output_tokens", 0) or 0,
        )

        return _llm.RealModelResult(
            content="".join(content_parts),
            raw_tool_calls=raw_tool_calls,
            usage=usage,
            next_hint_modality=next_hint_modality,
            next_hint_tier=next_hint_tier,
        )

    async def call_model_streaming(self, conversation, model, max_tokens, tools, on_chunk):
        from .. import llm as _llm

        system, messages = _openai_conversation_to_anthropic(conversation)

        content_buffer = ""
        unflushed = ""
        # Anthropic's stream sends content_block_start with block metadata,
        # then content_block_delta events keyed to that block's index.
        # For tool_use blocks the input arrives as a stream of
        # `input_json_delta` fragments — same JSON-fragment shape as
        # OpenAI's tool call streaming, just under a different event name.
        tool_blocks: dict[int, dict] = {}
        usage = Usage()

        async with self._client.messages.stream(
            model=model,
            max_tokens=max_tokens,
            system=system,
            messages=messages,
            tools=_openai_tools_to_anthropic(tools),
        ) as stream:
            async for event in stream:
                etype = getattr(event, "type", None)

                if etype == "content_block_start":
                    block = event.content_block
                    idx = event.index
                    if getattr(block, "type", None) == "tool_use":
                        tool_blocks[idx] = {"name": block.name, "arguments_frag": ""}

                elif etype == "content_block_delta":
                    delta = event.delta
                    dtype = getattr(delta, "type", None)
                    if dtype == "text_delta":
                        content_buffer += delta.text
                        unflushed += delta.text
                        while True:
                            boundary = sentence_segmenter.find_boundary(unflushed)
                            if boundary is None:
                                break
                            unflushed = unflushed[boundary:]
                            await on_chunk(content_buffer[: len(content_buffer) - len(unflushed)])
                    elif dtype == "input_json_delta":
                        idx = event.index
                        if idx in tool_blocks:
                            tool_blocks[idx]["arguments_frag"] += delta.partial_json or ""

                elif etype == "message_delta":
                    # Usage arrives here at the end of a stream. Anthropic
                    # only reports OUTPUT tokens on message_delta; input
                    # tokens were reported on the initial message_start
                    # captured above via the SDK's own state accessor.
                    du = getattr(event, "usage", None)
                    if du is not None:
                        # message_delta.usage carries incremental output_tokens
                        usage = Usage(
                            input_tokens=usage.input_tokens,
                            output_tokens=(du.output_tokens or 0),
                        )

            # After the stream is drained, pick up the full final message
            # for its accurate usage (input_tokens) — the SDK aggregates
            # this internally as get_final_message.
            final = await stream.get_final_message()
            fu = getattr(final, "usage", None)
            if fu is not None:
                usage = Usage(
                    input_tokens=getattr(fu, "input_tokens", 0) or 0,
                    output_tokens=getattr(fu, "output_tokens", 0) or 0,
                )

        if unflushed:
            await on_chunk(content_buffer)

        raw_tool_calls = []
        next_hint_modality, next_hint_tier = model_registry.default_hint()
        for idx in sorted(tool_blocks):
            tb = tool_blocks[idx]
            try:
                input_obj = json.loads(tb["arguments_frag"]) if tb["arguments_frag"] else {}
            except json.JSONDecodeError:
                logger.warning(
                    "AnthropicProvider.call_model_streaming: malformed tool_use input for %s, treating as empty",
                    tb["name"],
                )
                input_obj = {}
            if tb["name"] == _NEXT_STEP_HINT_TOOL_NAME:
                if isinstance(input_obj, dict):
                    next_hint_modality = input_obj.get("modality", next_hint_modality)
                    next_hint_tier = input_obj.get("tier", next_hint_tier)
                continue
            raw_tool_calls.append(
                {"name": tb["name"], "arguments": input_obj, "is_subagent": tb["name"] == _SPAWN_SUBAGENT_TOOL_NAME}
            )

        return _llm.RealModelResult(
            content=content_buffer,
            raw_tool_calls=raw_tool_calls,
            usage=usage,
            next_hint_modality=next_hint_modality,
            next_hint_tier=next_hint_tier,
        )

    async def summarize_text(self, system_prompt, user_content, model, max_tokens=None):
        # Anthropic requires max_tokens on every call; use a generous default
        # if the caller didn't specify one (this path is only used for
        # summarization, which by definition should be much shorter than
        # its input — 2048 is a reasonable safety ceiling).
        response = await self._client.messages.create(
            model=model,
            max_tokens=max_tokens or 2048,
            system=system_prompt,
            messages=[{"role": "user", "content": user_content}],
        )
        parts: list[str] = []
        for block in response.content:
            if getattr(block, "type", None) == "text":
                parts.append(block.text or "")
        return SimpleTextResult(content="".join(parts))

    async def close(self):
        await self._client.close()
