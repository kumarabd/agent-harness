"""Mirrors workflows/internal/ids/ids.go by hand — same scheme, Python side.

Under the reference-passing contract, ModelCall (not the workflow) mints
tool-call and subagent IDs, since it's the one writing the tool_calls row that
ID identifies (docs/components/temporal-workflow.md, "Resolved: Reference/ID
Schema"). This is the Python implementation of that minting; the Go side
(ids.go) documents the same format but no longer calls these functions itself
for fresh IDs — it only reuses IDs handed back in ModelCallOutput.
"""

from __future__ import annotations


def activity_id(turn_id: str, n: int) -> str:
    """"{turn_id}:act:{n}" — a plain tool call's fully-qualified activity ID."""
    return f"{turn_id}:act:{n}"


def subagent_turn_id(turn_id: str, n: int) -> str:
    """"{turn_id}:sub:{n}" — a subagent child workflow's ID, nested under its
    parent's turn ID. Recursion just keeps applying this."""
    return f"{turn_id}:sub:{n}"


def turn_id_of_tool_call(tool_call_id: str) -> str:
    """The inverse of `activity_id` — the turn_id (top-level or
    subagent-nested) that minted this tool_call_id. ":act:" only ever
    appears once, terminating the id (no turn_id itself contains it, at any
    nesting depth), so splitting on the last occurrence recovers the exact
    owner. Used by `tools.search_tools`'s mid-turn `turn_retrieval` binding
    (docs/components/tool-registry.md, "Resolved: Three-Layer Tool Taxonomy
    & Per-Task Resolution") — a tool handler only has its own ToolContext,
    not the turn_id directly."""
    turn_id, sep, _ = tool_call_id.rpartition(":act:")
    if not sep:
        raise ValueError(f"not a tool_call_id (missing ':act:'): {tool_call_id!r}")
    return turn_id


def session_key_of(turn_id: str) -> str:
    """The session_key prefix of any turn_id (top-level or subagent) — the
    same split session_fs_path uses, exposed separately since callers
    coordinating leases (leases.py) need the raw session_key too, not just
    the derived path."""
    session_key, sep, _ = turn_id.partition(":turn:")
    if not sep:
        raise ValueError(f"not a turn_id (missing ':turn:'): {turn_id!r}")
    return session_key


def user_scope_of(session_key: str) -> str:
    """The user-stable scope a standing intention keys on
    (docs/components/proactivity.md).

    `session_key` is deliberately channel/branch-scoped, not user-scoped
    (gateway core.SessionKeyFor) — for web it embeds the user
    ("agent:main:web:user:<id>"); for a shared Discord channel the channel is
    the best available scope. Stripping any per-branch ":session:<id>" or
    per-thread ":thread:<id>" suffix gives a stable namespace shared across a
    user's branches/threads, and the result is always itself a valid canonical
    session_key — so it also names the session a fired intention wakes.
    """
    for marker in (":session:", ":thread:"):
        head, sep, _ = session_key.partition(marker)
        if sep:
            return head
    return session_key


def session_fs_path(turn_id: str) -> str:
    """Maps a turn_id to its working directory on the session filesystem PV,
    per docs/components/session-filesystem.md's path convention:
    "/session/{session_key}/" for a top-level turn, extended with "sub/{n}/"
    per nesting level for a subagent. No new schema/queries needed — the ID
    scheme was designed hierarchically precisely so this is a pure string
    transform: "sess-1:turn:1" -> "/session/sess-1/"; "sess-1:turn:1:sub:1"
    -> "/session/sess-1/sub/1/"; "sess-1:turn:1:sub:1:sub:2" ->
    "/session/sess-1/sub/1/sub/2/".

    Only ever called with an actual turn_id (a tool_calls row's parent_id,
    which is always a turn or subagent-turn ID, never an ":act:" activity
    ID) — not meant for arbitrary strings.
    """
    session_key, sep, rest = turn_id.partition(":turn:")
    if not sep:
        raise ValueError(f"not a turn_id (missing ':turn:'): {turn_id!r}")
    parts = rest.split(":")
    sub_parts = parts[1:]  # parts[0] is the top-level turn_seq — irrelevant to the path
    path = f"/session/{session_key}/"
    for i in range(0, len(sub_parts), 2):
        marker, n = sub_parts[i], sub_parts[i + 1]
        if marker != "sub":
            raise ValueError(f"unexpected turn_id segment {marker!r} in {turn_id!r}")
        path += f"sub/{n}/"
    return path
