"""Provider ABC — the shape every provider implementation implements.

Internal conversation format is OpenAI-shaped (list of {role, content,
tool_calls?, tool_call_id?} dicts) — every caller
(lcm.py's assemble/compact, model_call.py's build path, exploration_summary)
builds and stores messages in this shape, because that's what
Postgres.messages plus tool_calls already looks like and what
llm.TOOLS_SCHEMA is defined against. Each Provider translates from this
shape to its own API's shape at the call boundary and back again — the
adaptation layer lives inside the provider, not sprinkled across every
caller.

Two entry points:

1. call_model / call_model_streaming — full agent conversation with
   tools, hint extraction, usage accounting, and (for streaming) sentence
   -boundary chunking. Returns RealModelResult.

2. summarize_text — the simpler "system + user, no tools, no streaming,
   return the message text" shape lcm.compact and
   exploration_summary._summarize_text both use.

Providers own client construction internally so the (base_url, api_key)
they were built with is genuinely per-instance state — llm_client just
caches the Provider itself, keyed on (provider, base_url, api_key).
"""

from __future__ import annotations

from abc import ABC, abstractmethod
from dataclasses import dataclass


@dataclass
class SimpleTextResult:
    """Return shape of summarize_text — deliberately much smaller than
    RealModelResult since none of the tool/hint/streaming machinery
    matters for a simple summarize call."""

    content: str


class Provider(ABC):
    """One provider's worth of API-shape knowledge. See module docstring."""

    @abstractmethod
    async def call_model(
        self,
        conversation: list[dict],
        model: str,
        max_tokens: int,
        tools: list[dict],
    ):
        """Full agent-conversation call, non-streaming. Returns
        RealModelResult (from llm.py — passing that type in via return
        rather than import to keep this module import-clean of llm.py,
        avoiding a circular reference)."""

    @abstractmethod
    async def call_model_streaming(
        self,
        conversation: list[dict],
        model: str,
        max_tokens: int,
        tools: list[dict],
        on_chunk,
    ):
        """Streaming counterpart to call_model. on_chunk is an async
        callable invoked with cumulative text at sentence boundaries.
        See llm.call_model_streaming for the delivery contract callers
        depend on."""

    @abstractmethod
    async def summarize_text(
        self,
        system_prompt: str,
        user_content: str,
        model: str,
        max_tokens: int | None = None,
    ) -> SimpleTextResult:
        """Simple system+user prompt → text. No tools, no streaming.
        Used by lcm.compact (context compression) and
        exploration_summary._summarize_text (large-output description)
        — both cases where the full call_model machinery is overkill."""

    @abstractmethod
    async def close(self) -> None:
        """Shutdown hook — close any underlying HTTP client. Called by
        llm_client.close_all() at worker shutdown."""
