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
from urllib.parse import quote

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


class McpHubNotConfiguredError(RuntimeError):
    pass


class McpHubCallError(RuntimeError):
    pass


def _base_url() -> str:
    base_url = os.environ.get("MCP_HUB_URL", "").rstrip("/")
    if not base_url:
        raise McpHubNotConfiguredError("MCP_HUB_URL is not set")
    return base_url


def _mcp_url() -> str:
    return f"{_base_url()}/mcp"


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


# Subscriptions go over mcp-hub's management API rather than its MCP endpoint. A
# subscription is not a tool the agent reaches — it is plumbing between the harness
# and the hub, and `/api/connections` is managed through the same surface. The two
# stay separate deliberately: MCP here is the tool-broker face, this is the
# configuration face.
#
# `event_key` is opaque in both directions. The harness never parses it, mcp-hub
# never interprets it, and the engine that raises it is the only thing that knows
# what it means — which is what lets one delivery loop serve every connection.


async def _request(method: str, path: str, **kwargs: Any) -> httpx.Response:
    """One place where transport failures become this module's own error type.

    Without it a timeout escapes as a raw `httpx` exception, which every caller
    would have to know about — and the ones that only meant to tolerate a hub being
    unconfigured would instead fail outright on a hub being briefly unreachable.
    After this, a caller needs exactly two cases: not configured, or call failed.
    """
    try:
        async with httpx.AsyncClient(timeout=10.0) as client:
            return await client.request(method, f"{_base_url()}{path}", **kwargs)
    except McpHubNotConfiguredError:
        raise
    except httpx.HTTPError as exc:
        raise McpHubCallError(f"{method} {path}: {exc}") from exc


async def arm_subscription(wake_id: str, event_key: str, invoke_id: str) -> dict[str, Any]:
    """Register what an `event_key` should wake.

    Called *before* anything subscribes to the underlying event, so a notice can
    never arrive before something can route it. In practice that ordering is now
    free rather than required — an engine holds notices until they are acked, so a
    subscription armed late still receives what it missed.
    """
    response = await _request(
        "POST",
        "/api/subscriptions",
        json={"wake_id": wake_id, "event_key": event_key, "invoke_id": invoke_id},
    )
    if response.status_code >= 400:
        raise McpHubCallError(f"arm_subscription: HTTP {response.status_code} {response.text.strip()[:200]}")
    return response.json()


async def list_subscriptions() -> list[dict[str, Any]]:
    response = await _request("GET", "/api/subscriptions")
    if response.status_code >= 400:
        raise McpHubCallError(f"list_subscriptions: HTTP {response.status_code}")
    return response.json().get("items", [])


async def cancel_subscription(wake_id: str) -> bool:
    """Disarm. False means nothing was armed under that id — a real answer the
    caller has to be able to give, rather than reporting success either way."""
    response = await _request("DELETE", f"/api/subscriptions/{quote(wake_id, safe='')}")
    if response.status_code == 404:
        return False
    if response.status_code >= 400:
        raise McpHubCallError(f"cancel_subscription: HTTP {response.status_code}")
    return True
