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
    # A plain file on the tenant's own PV (deploy/helm/agent-harness-tenant's
    # tenantVolume, TOOL_PERMISSION_LIST_FILE) rather than a Helm-templated
    # env var — nothing tenant-identity-shaped about a mutable policy list,
    # and a Helm value would just be a second, driftable copy of whatever's
    # actually on disk. Edited directly at runtime; missing file (the normal
    # starting state — nothing pre-populates it) or invalid JSON both mean
    # empty, not an error, same tolerance the old env-var version had for a
    # missing/malformed TOOL_PERMISSION_LIST.
    path = os.environ.get("TOOL_PERMISSION_LIST_FILE", "")
    if not path:
        return []
    try:
        with open(path, encoding="utf-8") as f:
            entries = json.load(f)
    except (OSError, json.JSONDecodeError):
        return []
    return [PermissionRule(server=e["server"], tool=e["tool"]) for e in entries if "server" in e and "tool" in e]


PERMISSION_LIST: list[PermissionRule] = _load_permission_list()


def requires_approval(server: str, tool: str) -> bool:
    return any(r.server == server and r.tool == tool for r in PERMISSION_LIST)
