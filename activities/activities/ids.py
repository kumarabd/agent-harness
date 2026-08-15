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
