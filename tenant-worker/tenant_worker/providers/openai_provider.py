"""OpenAI-compatible provider — covers real OpenAI, DeepSeek, Qwen/
DashScope, Groq, OpenRouter, Crusoe, and any other endpoint speaking
the OpenAI chat.completions protocol.

Owns nothing about the specific vendor — the differences are entirely
`base_url` + `model` name + `api_key`, all passed in from ModelConfig.
"""

from __future__ import annotations

import json
import logging

from openai import AsyncOpenAI

from .. import sentence_segmenter
from ..types import Usage
from .base import REPORT_STATUS_TOOL_NAME, Provider, SimpleTextResult, parse_report_status

logger = logging.getLogger(__name__)

# The peeled meta-tool the model authors turn-pipeline.md's status + next_step
# with — defined in base.py (which llm.py can't be imported from here without a
# cycle). This provider strips it from raw_tool_calls before they reach the
# caller and returns the parsed fields on RealModelResult.
_REPORT_STATUS_TOOL_NAME = REPORT_STATUS_TOOL_NAME

# docs/components/temporal-workflow.md's recursion-termination guard —
# is_subagent has to be derived from which tool the model actually called,
# not hardcoded False. Same duplication reasoning as above.
_SPAWN_SUBAGENT_TOOL_NAME = "spawn_subagent"


class OpenAIProvider(Provider):
    def __init__(self, base_url: str, api_key: str):
        self._client = AsyncOpenAI(base_url=base_url, api_key=api_key)

    async def call_model(self, conversation, model, max_tokens, tools):
        # Deferred import to avoid the llm.py ↔ providers circular that
        # would otherwise land — llm.py imports from providers via
        # llm_client, so providers can't import from llm.py at module
        # load time. RealModelResult only crosses the return boundary,
        # never held as a field, so a function-scope import is fine.
        from .. import llm as _llm

        response = await self._client.chat.completions.create(
            model=model,
            messages=conversation,
            tools=tools,
            max_tokens=max_tokens,
        )
        message = response.choices[0].message

        raw_tool_calls = []
        reported = None
        for tc in message.tool_calls or []:
            if tc.function.name == _REPORT_STATUS_TOOL_NAME:
                try:
                    reported = parse_report_status(json.loads(tc.function.arguments))
                except (json.JSONDecodeError, AttributeError):
                    logger.warning("OpenAIProvider.call_model: malformed %s arguments", _REPORT_STATUS_TOOL_NAME)
                continue
            raw_tool_calls.append(
                {
                    "name": tc.function.name,
                    "arguments": json.loads(tc.function.arguments),
                    "is_subagent": tc.function.name == _SPAWN_SUBAGENT_TOOL_NAME,
                }
            )

        usage = Usage(
            input_tokens=response.usage.prompt_tokens if response.usage else 0,
            output_tokens=response.usage.completion_tokens if response.usage else 0,
        )
        return _llm.RealModelResult(
            content=message.content or "",
            raw_tool_calls=raw_tool_calls,
            usage=usage,
            status=reported.status if reported else "",
            next_hint_tier=reported.tier if reported else "",
            next_step_note=reported.note if reported else "",
            est_remaining_steps=reported.est_remaining_steps if reported else 0,
        )

    async def call_model_streaming(self, conversation, model, max_tokens, tools, on_chunk):
        from .. import llm as _llm

        stream = await self._client.chat.completions.create(
            model=model,
            messages=conversation,
            tools=tools,
            max_tokens=max_tokens,
            stream=True,
            stream_options={"include_usage": True},
        )

        content_buffer = ""
        unflushed = ""
        # Tool calls accumulate by index — OpenAI's streaming shape sends
        # each tool call's name/arguments as fragments across multiple
        # chunks, matched by the delta's own index, never assumed to
        # arrive whole.
        tool_call_frags: dict[int, dict] = {}
        usage = Usage()

        async for chunk in stream:
            if chunk.usage is not None:
                usage = Usage(
                    input_tokens=chunk.usage.prompt_tokens or 0,
                    output_tokens=chunk.usage.completion_tokens or 0,
                )
            if not chunk.choices:
                continue
            delta = chunk.choices[0].delta
            if delta.content:
                content_buffer += delta.content
                unflushed += delta.content
                while True:
                    boundary = sentence_segmenter.find_boundary(unflushed)
                    if boundary is None:
                        break
                    unflushed = unflushed[boundary:]
                    await on_chunk(content_buffer[: len(content_buffer) - len(unflushed)])
            for tc in delta.tool_calls or []:
                frag = tool_call_frags.setdefault(tc.index, {"name": "", "arguments": ""})
                if tc.function and tc.function.name:
                    frag["name"] += tc.function.name
                if tc.function and tc.function.arguments:
                    frag["arguments"] += tc.function.arguments

        if unflushed:
            # Final forced flush — the last delivered chunk must always
            # equal the complete response, regardless of whether it ends
            # on a real sentence boundary.
            await on_chunk(content_buffer)

        raw_tool_calls = []
        reported = None
        for idx in sorted(tool_call_frags):
            frag = tool_call_frags[idx]
            if frag["name"] == _REPORT_STATUS_TOOL_NAME:
                try:
                    reported = parse_report_status(json.loads(frag["arguments"]))
                except (json.JSONDecodeError, AttributeError):
                    logger.warning(
                        "OpenAIProvider.call_model_streaming: malformed %s arguments", _REPORT_STATUS_TOOL_NAME
                    )
                continue
            try:
                arguments = json.loads(frag["arguments"]) if frag["arguments"] else {}
            except json.JSONDecodeError:
                logger.warning(
                    "OpenAIProvider.call_model_streaming: malformed tool call arguments for %s, treating as empty",
                    frag["name"],
                )
                arguments = {}
            raw_tool_calls.append(
                {"name": frag["name"], "arguments": arguments, "is_subagent": frag["name"] == _SPAWN_SUBAGENT_TOOL_NAME}
            )

        return _llm.RealModelResult(
            content=content_buffer,
            raw_tool_calls=raw_tool_calls,
            usage=usage,
            status=reported.status if reported else "",
            next_hint_tier=reported.tier if reported else "",
            next_step_note=reported.note if reported else "",
            est_remaining_steps=reported.est_remaining_steps if reported else 0,
        )

    async def summarize_text(self, system_prompt, user_content, model, max_tokens=None):
        kwargs = {
            "model": model,
            "messages": [
                {"role": "system", "content": system_prompt},
                {"role": "user", "content": user_content},
            ],
        }
        if max_tokens is not None:
            kwargs["max_tokens"] = max_tokens
        response = await self._client.chat.completions.create(**kwargs)
        return SimpleTextResult(content=response.choices[0].message.content or "")

    async def close(self):
        await self._client.close()
