"""Prompt assembly — docs/components/turn-pipeline.md's prompt-assembly section.

  prompt = static core (the system prompt) + pinned scratchpad + LCM conversation

`lcm.assemble` builds the conversation (system prompt + summary DAG + verbatim
window); everything the model retrieves at runtime (search_memory, discover_tools, a
tool result) flows back into that stream as an ordinary observation, so there is
no separate managed "memory" / "skills" section here any more. The one thing that
isn't conversation is the callable tool schemas: `discover_tools`'s staged rows
become directly-callable `Capability` objects for `tools_schema_for`.

Runs inside `ModelCall`, every call; `llm.build_conversation` is the stable call
site.
"""

from __future__ import annotations

import json
import logging
import os

from . import capabilities, ids, lcm

logger = logging.getLogger(__name__)

_SCRATCHPAD_HEADER = (
    "Your working scratchpad for this task (you maintain this file yourself — "
    "write your plan, progress, and findings there for anything multi-step; it "
    "is kept in front of you every step and never compacted):\n\n"
)


async def assemble(
    conn, turn_id: str, system_prompt: str,
) -> tuple[list[dict], int, list[capabilities.Capability]]:
    """Returns (conversation, context_tokens, resolved_tools).

    `context_tokens` is threaded back through ModelCallOutput for the
    compression-gate check. `resolved_tools` is `discover_tools`'s staged
    `turn_retrieval` rows (owner_id = turn_id, kind='tool') turned into
    directly-callable `Capability` objects.
    """
    session_key = ids.session_key_of(turn_id)
    conversation, context_tokens = await lcm.assemble(conn, session_key, system_prompt)

    # Pinned: the scratchpad file, verbatim, right after the system prompt.
    scratchpad = _scratchpad_text(turn_id)
    if scratchpad:
        conversation.insert(1, {"role": "system", "content": scratchpad})
        context_tokens += lcm.estimate_tokens(scratchpad)

    tool_rows = await _staged_tool_rows(conn, turn_id)
    resolved_tools = capabilities.mint_resolved(tool_rows)

    if scratchpad or resolved_tools:
        logger.info(
            "prompt.assemble[%s]: scratchpad=%s resolved_tools=%d ctx_tokens=%d",
            turn_id, bool(scratchpad), len(resolved_tools), context_tokens,
        )
    return conversation, context_tokens, resolved_tools


def _scratchpad_text(turn_id: str) -> str | None:
    """The turn's scratchpad file if the model has written one. Co-located with
    the turn's own shell working directory (a top-level turn and each subagent
    get their own), so it survives across a session's turns and stays isolated
    per subagent. SESSION_ROOT read at point of use, same convention as
    tools.resolve_session_dir (no shared config module)."""
    root = os.environ.get("SESSION_ROOT", "/tmp/agent-harness-sessions")
    path = os.path.join(root, ids.session_fs_path(turn_id).lstrip("/"), "scratchpad.md")
    try:
        with open(path, encoding="utf-8") as f:
            content = f.read().strip()
    except OSError:
        return None
    return (_SCRATCHPAD_HEADER + content) if content else None


async def _staged_tool_rows(conn, turn_id: str) -> list[tuple[str, dict]]:
    """`discover_tools`'s staged `(content, metadata)` rows for this turn —
    `metadata` carries {server, tool, input_schema} (jsonb; asyncpg returns it
    as text)."""
    rows = await conn.fetch(
        "SELECT content, metadata FROM turn_retrieval "
        "WHERE owner_id = $1 AND kind = 'tool' ORDER BY seq",
        turn_id,
    )
    return [(r["content"], json.loads(r["metadata"]) if r["metadata"] else {}) for r in rows]
