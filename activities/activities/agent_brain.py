"""Thin async clients for agent-brain's two separate MCP servers (docs/components/
memory-slot.md — "This component *is* the agent-brain integration, directly"). No
generic backend abstraction: this module calls each server's tools by name, over MCP's
streamable-HTTP transport.

agent-brain now ships as two independent processes with two independent tool surfaces,
not one:

- The **Go server** — `memory_write`/`memory_system_status`/`memory_audit_tail`. Nothing
  in this project calls it after the retain/recall/reflect rewrite (`memory_write` is a
  dead end on agent-brain's own side — nothing downstream extracts from it anymore), but
  `call_tool`/`AGENT_BRAIN_BASE_URL`/`AGENT_BRAIN_API_KEY` are kept as-is rather than
  ripped out this pass, in case ops tooling still wants `memory_system_status`/
  `memory_audit_tail` directly.
- The **retain MCP server** (`mcp_server.py`) — `memory_retain`/`memory_recall`/
  `memory_reflect`/mental-models. This is the one `tools.py`/`write_memory.py` actually
  use now, via `call_retain_tool`.

Config read from env vars at point of use, same convention as tools.py's
resolve_session_dir — no shared config module:

    AGENT_BRAIN_BASE_URL          Go server, e.g. http://<release>-agent-brain-server:8080
    AGENT_BRAIN_API_KEY           Go server's X-API-Key header.
    AGENT_BRAIN_AGENT_ID          Go server's X-Agent-ID header. Doubles as the retain
                                   server's bank_id (see retain_bank_id()) — one bank
                                   per tenant, and this value already identifies the
                                   tenant/deployment the same way.
    AGENT_BRAIN_RETAIN_BASE_URL   retain MCP server, e.g.
                                   http://<release>-agent-brain-retain-mcp:8890
    AGENT_BRAIN_RETAIN_API_KEY    retain server's X-API-Key header (empty if the
                                   deployment runs it with no auth check).

Raises AgentBrainNotConfiguredError if the relevant base URL isn't set, so a call site
can distinguish "memory isn't configured for this deployment" from a real call failure.
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


def retain_bank_id() -> str:
    """bank_id sent on every retain-server call — one bank per tenant, reusing the
    tenant/deployment identifier already carried by AGENT_BRAIN_AGENT_ID rather than
    introducing a second config value for the same concept."""
    return os.environ.get("AGENT_BRAIN_AGENT_ID", "")


async def _call(url: str, headers: dict[str, str], tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Opens a fresh MCP session per call rather than holding one open across the
    worker's lifetime — these are infrequent, latency-tolerant calls (a session-start
    retrieval, a mid-turn tool call, a fire-and-forget write), not a hot path where
    connection reuse would matter; a fresh session also sidesteps any
    session-affinity/expiry handling this module would otherwise need to get right."""
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


async def call_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Calls a tool on the Go server (memory_write/memory_system_status/memory_audit_tail)."""
    base_url = os.environ.get("AGENT_BRAIN_BASE_URL", "").rstrip("/")
    if not base_url:
        raise AgentBrainNotConfiguredError("AGENT_BRAIN_BASE_URL is not set")
    headers = {
        "X-API-Key": os.environ.get("AGENT_BRAIN_API_KEY", ""),
        "X-Agent-ID": os.environ.get("AGENT_BRAIN_AGENT_ID", ""),
    }
    return await _call(f"{base_url}/mcp", headers, tool_name, arguments)


async def call_retain_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Calls a tool on the retain MCP server (memory_retain/memory_recall/
    memory_reflect/mental models). Callers pass bank_id explicitly in arguments —
    use retain_bank_id() to fill it in, this function doesn't inject it implicitly."""
    base_url = os.environ.get("AGENT_BRAIN_RETAIN_BASE_URL", "").rstrip("/")
    if not base_url:
        raise AgentBrainNotConfiguredError("AGENT_BRAIN_RETAIN_BASE_URL is not set")
    headers = {"X-API-Key": os.environ.get("AGENT_BRAIN_RETAIN_API_KEY", "")}
    return await _call(f"{base_url}/mcp", headers, tool_name, arguments)


_PERSONA_MENTAL_MODEL_ID = "persona"


async def ensure_persona_mental_model() -> None:
    """Idempotent per-tenant bootstrap step, called once at tenant-worker startup
    (tenant_worker.py's main()) — docs/components/memory-slot.md, "Resolved:
    Persona/Directive Content via Mental Models". Replaces the old design's
    write-time `harness_type: persona-rule` tagging (which depended on agent-brain's
    now-dropped `facts`/`entity_identities` tables) with a `subtype="directive"`
    mental model: bank-scoped, unconditionally injected into every memory_reflect
    call for this tenant (retain/db.py's fetch_directives), refreshed automatically
    as new content is retained (refresh_after_retain=True).

    Checked via memory_list_mental_models rather than racing
    memory_create_mental_model's own UNIQUE-violation on a duplicate id — the server
    propagates that uncaught (not as a friendly {"error": ...} dict), so detecting
    "already exists" would mean depending on undocumented error text instead of a
    real read.
    """
    bank_id = retain_bank_id()
    if not bank_id:
        return
    existing = await call_retain_tool("memory_list_mental_models", {"bank_id": bank_id})
    if any(m["id"] == _PERSONA_MENTAL_MODEL_ID for m in existing.get("mental_models", [])):
        return
    await call_retain_tool(
        "memory_create_mental_model",
        {
            "bank_id": bank_id,
            "mental_model_id": _PERSONA_MENTAL_MODEL_ID,
            "name": "Persona",
            "source_query": (
                "persistent rules, preferences, and persona facts about how to work with this user/tenant"
            ),
            "subtype": "directive",
            "refresh_after_retain": True,
        },
    )
