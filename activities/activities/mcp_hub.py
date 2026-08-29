"""Thin async client for mcp-hub's MCP endpoint (docs/components/tool-registry.md,
"Resolved: mcp-hub-Mediated Integration Mechanism"). Same shape as agent_brain.py,
simpler: mcp-hub's own server (/Users/abishekkumar/Documents/infra/mcp-hub/src/mcp_hub/server.py,
verified directly) has no incoming authentication of its own — isolation is
per-tenant pod/network boundaries, not a credential — so this client sends no
headers at all, unlike agent_brain.py's X-API-Key/X-Agent-ID.

Config read from env vars at point of use, same convention as
resolve_session_dir/agent_brain.py:

    MCP_HUB_URL   e.g. http://<release>:8000 (deploy/helm/agent-harness-tenant's
                  templates/tenant-worker-deployment.yaml — mcp-hub's own chart
                  computes its Service name as just .Release.Name, no suffix).
                  This module appends /mcp itself.
"""

from __future__ import annotations

import json
import os
from typing import Any

from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


class McpHubNotConfiguredError(RuntimeError):
    pass


class McpHubCallError(RuntimeError):
    pass


def _mcp_url() -> str:
    base_url = os.environ.get("MCP_HUB_URL", "").rstrip("/")
    if not base_url:
        raise McpHubNotConfiguredError("MCP_HUB_URL is not set")
    return f"{base_url}/mcp"


async def call_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any] | list[Any] | str:
    """Calls one mcp-hub MCP tool (search_tools, call_tool, search_skills, or
    get_skill) and returns its result — parsed JSON for a structured result
    (every tool but get_skill), or the raw string for a tool whose real
    content is plain text (get_skill). Fresh session per call — see
    agent_brain.py's call_tool for the same reasoning."""
    url = _mcp_url()
    async with streamable_http_client(url) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            result = await session.call_tool(tool_name, arguments)

    if result.is_error:
        message = result.content[0].text if result.content else "unknown error"
        raise McpHubCallError(f"{tool_name}: {message}")

    if result.structured_content is not None:
        return result.structured_content
    if result.content and hasattr(result.content[0], "text"):
        text = result.content[0].text
        # docs/components/skills.md — get_skill's real return (verified
        # directly against mcp-hub's own server.py) is a plain string (skill
        # content, verbatim Markdown/text), not a JSON-serialized object the
        # way search_tools/call_tool's own results always are — every prior
        # caller of this function only ever got structured (dict/list)
        # results back, so json.loads unconditionally was never wrong until
        # now. Try JSON first (preserves existing search_tools/call_tool
        # behavior exactly, byte-for-byte), fall back to the raw string for
        # a tool whose real content was never JSON to begin with — general
        # robustness for this shared client, not a get_skill-specific
        # special case, since any future plain-text-returning mcp-hub tool
        # hits the same fallback for free.
        try:
            return json.loads(text)
        except json.JSONDecodeError:
            return text
    raise McpHubCallError(f"{tool_name}: response had no content")
