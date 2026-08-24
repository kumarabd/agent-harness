"""Permission gating — docs/components/user-input.md, "Resolved: Permission
Gating as the First Consumer". Fully decoupled from both tool sources:
NOT a property on shell_hub.py's catalog entries, NOT a property on any
mcp_hub.py tool metadata, and NOT stored anywhere near either module's
embedding/search index. This is a standalone permission list, consulted only
by the two real execution paths (shell_exec, call_tool — see tools.py), never
by search_tools. A tool's own discovery/definition (intrinsic, owned by
whoever built the catalog) and a deployment's approval policy for it
(extrinsic, tenant-specific) are deliberately kept as two separate concerns
with two separate owners.

Reuses the exact {server, tool} identity vocabulary search_tools/call_tool
already use — server="shell" for shell-sourced commands, matching
shell_hub.search()'s own result shape.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass


@dataclass(frozen=True)
class PermissionRule:
    server: str  # "shell" | an mcp-hub server name
    tool: str  # command name (shell) | tool name (mcp-hub)


def _load_permission_list() -> list[PermissionRule]:
    # Deployment-config-style, same convention as shell_hub.py's
    # MANUAL_OVERRIDES / model_registry.py's env-var storage — not a
    # Postgres-backed policy engine. Empty by default: no tools populated
    # yet, deliberately (docs/components/user-input.md).
    raw = os.environ.get("TOOL_PERMISSION_LIST", "[]")
    try:
        entries = json.loads(raw)
    except json.JSONDecodeError:
        return []
    return [PermissionRule(server=e["server"], tool=e["tool"]) for e in entries if "server" in e and "tool" in e]


PERMISSION_LIST: list[PermissionRule] = _load_permission_list()


def requires_approval(server: str, tool: str) -> bool:
    return any(r.server == server and r.tool == tool for r in PERMISSION_LIST)
