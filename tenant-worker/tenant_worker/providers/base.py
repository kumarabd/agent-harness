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

import logging
from abc import ABC, abstractmethod
from dataclasses import dataclass

logger = logging.getLogger(__name__)

# docs/components/turn-pipeline.md, "Model I/O schema" — the model authors
# `status` + `next_step` via this peeled meta-tool. No provider guarantees
# native structured output, so a peeled tool call (never dispatched, never
# shown to the user) is the transport; semantically it IS the output field.
# Replaces the Phase-2-era `declare_next_step_hint` (tier hint only).
#
# Lives here, not in llm.py, because llm.py imports providers (via
# llm_client) — a provider importing back from llm.py would circular. Both
# providers peel this name from their raw tool calls before returning.
REPORT_STATUS_TOOL_NAME = "report_status"

_VALID_STATUSES = ("working", "done", "blocked")
_VALID_TIERS = ("fast", "medium", "expert")


@dataclass
class ReportStatus:
    """Parsed `report_status` payload. `status` empty ⇒ the model didn't call
    it this step; model_call.py synthesizes from tool-call presence (the
    Phase-2 fallback, removed in Phase 9)."""

    status: str = ""
    tier: str = ""
    note: str = ""
    est_remaining_steps: int = 0


def parse_report_status(args: object) -> ReportStatus:
    """Tolerant parse of a `report_status` tool call's arguments (a dict on
    both provider shapes by the time it reaches here). Unknown/malformed
    fields are dropped, not raised on — a weak model half-complying is
    common and must not fail the turn."""
    if not isinstance(args, dict):
        logger.warning("parse_report_status: non-dict args %r, ignoring", type(args))
        return ReportStatus()
    status = str(args.get("status", "")).strip().lower()
    if status not in _VALID_STATUSES:
        status = ""
    tier = str(args.get("tier", "")).strip().lower()
    if tier not in _VALID_TIERS:
        tier = ""
    try:
        est = int(args.get("est_remaining_steps", 0) or 0)
    except (TypeError, ValueError):
        est = 0
    return ReportStatus(
        status=status,
        tier=tier,
        note=str(args.get("note", "")).strip(),
        est_remaining_steps=max(0, est),
    )


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
