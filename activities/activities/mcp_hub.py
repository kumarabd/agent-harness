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


async def call_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any] | list[Any]:
    """Calls one mcp-hub MCP tool (search_tools or call_tool) and returns its
    parsed JSON result. Fresh session per call — see agent_brain.py's
    call_tool for the same reasoning."""
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
        return json.loads(result.content[0].text)
    raise McpHubCallError(f"{tool_name}: response had no content")
