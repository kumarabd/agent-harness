"""Thin async client for agent-brain's MCP endpoint (docs/components/memory-slot.md
— "This component *is* the agent-brain integration, directly"). No generic
backend abstraction: this module calls agent-brain's own `memory_search`/
`memory_expand`/`memory_write` tools by name, over MCP's streamable-HTTP
transport (agent-brain's `/mcp` endpoint — the same server also serves classic
SSE at `/sse`, but streamable-HTTP is a plain request/response POST per call,
no persistent stream to manage for the single-call usage this module needs).

Config read from env vars at point of use, same convention as tools.py's
resolve_session_dir — no shared config module:

    AGENT_BRAIN_BASE_URL   e.g. http://<release>-agent-brain-server:8080
                            (NOT the /sse suffix other env wiring in this
                            project uses for a different transport — this
                            module appends /mcp itself).
    AGENT_BRAIN_API_KEY    X-API-Key header (docs/components/memory-slot.md,
                            "Resolved: Per-Tenant Auth").
    AGENT_BRAIN_AGENT_ID   X-Agent-ID header — REQUIRED by agent-brain's own
                            auth (internal/auth/credentials.go,
                            internal/mcp/tenant.go's "agent_id required"
                            check) whenever AUTH_REQUIRED=true. Identifies
                            this calling product, not a per-tenant or
                            per-session value — "the same for every install"
                            per agent-brain's own domain.TenantContext.AgentID
                            comment.

Raises AgentBrainNotConfiguredError if AGENT_BRAIN_BASE_URL isn't set, so a
call site can distinguish "memory isn't configured for this deployment" from
a real call failure.
"""

from __future__ import annotations

import json
import os
from typing import Any

from mcp import ClientSession
from mcp.client.streamable_http import create_mcp_http_client, streamable_http_client


class AgentBrainNotConfiguredError(RuntimeError):
    pass


class AgentBrainCallError(RuntimeError):
    pass


def _mcp_url() -> str:
    base_url = os.environ.get("AGENT_BRAIN_BASE_URL", "").rstrip("/")
    if not base_url:
        raise AgentBrainNotConfiguredError("AGENT_BRAIN_BASE_URL is not set")
    return f"{base_url}/mcp"


async def call_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Calls one agent-brain MCP tool and returns its parsed JSON result.

    Opens a fresh session per call rather than holding one open across the
    worker's lifetime — these are infrequent, latency-tolerant calls (a
    session-start retrieval, a mid-turn tool call, a fire-and-forget write),
    not a hot path where connection reuse would matter; a fresh session also
    sidesteps any session-affinity/expiry handling this module would
    otherwise need to get right.
    """
    url = _mcp_url()
    headers = {
        "X-API-Key": os.environ.get("AGENT_BRAIN_API_KEY", ""),
        "X-Agent-ID": os.environ.get("AGENT_BRAIN_AGENT_ID", ""),
    }
    http_client = create_mcp_http_client(headers=headers)
    async with streamable_http_client(url, http_client=http_client) as (read, write):
        async with ClientSession(read, write) as session:
            await session.initialize()
            result = await session.call_tool(tool_name, arguments)

    if result.is_error:
        message = result.content[0].text if result.content else "unknown error"
        raise AgentBrainCallError(f"{tool_name}: {message}")

    if result.structured_content is not None:
        return result.structured_content
    if result.content and hasattr(result.content[0], "text"):
        return json.loads(result.content[0].text)
    raise AgentBrainCallError(f"{tool_name}: response had no content")
