"""Thin async client for agent-brain's retain MCP server (docs/components/
memory-slot.md — "This component *is* the agent-brain integration, directly"). No
generic backend abstraction: this module calls that server's tools by name, over MCP's
streamable-HTTP transport.

**2026-09-13: the Go server connection is gone, not just unused.** agent-brain's Go
server (`memory_write`/`memory_system_status`/`memory_audit_tail`) is a separate process
this project no longer talks to at all — `memory_write` was a dead end on agent-brain's
own side even before this (nothing downstream extracted from it anymore) once the
retain/recall/reflect rewrite landed. Removed outright rather than kept-but-unused: the
old `call_tool`/`AGENT_BRAIN_BASE_URL`/`AGENT_BRAIN_API_KEY` client, and the matching
Helm env/secret wiring (`docs/components/memory-slot.md`'s Notes Log). The Go server
itself still exists and still serves `memory_system_status`/`memory_audit_tail`/`agent-web`'s
own Explorer UI — just not called from this codebase.

Config read from env vars at point of use, same convention as tools.py's
resolve_session_dir — no shared config module:

    AGENT_BRAIN_RETAIN_BASE_URL   retain MCP server, e.g.
                                   http://<release>-agent-brain-retain-mcp:8890
    AGENT_BRAIN_RETAIN_API_KEY    retain server's X-API-Key header (empty if the
                                   deployment runs it with no auth check).
    AGENT_BRAIN_AGENT_ID          Doubles as the retain server's bank_id (see
                                   retain_bank_id()) — one bank per tenant, and this
                                   value already identifies the tenant/deployment.

Raises AgentBrainNotConfiguredError if AGENT_BRAIN_RETAIN_BASE_URL isn't set, so a call
site can distinguish "memory isn't configured for this deployment" from a real call
failure.
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


async def call_retain_tool(tool_name: str, arguments: dict[str, Any]) -> dict[str, Any]:
    """Calls a tool on the retain MCP server (memory_retain/memory_recall/
    memory_reflect/mental models) and returns its parsed JSON result. Callers pass
    bank_id explicitly in arguments — use retain_bank_id() to fill it in, this function
    doesn't inject it implicitly.

    Opens a fresh MCP session per call rather than holding one open across the worker's
    lifetime — these are infrequent, latency-tolerant calls (a mid-turn tool call, a
    fire-and-forget write), not a hot path where connection reuse would matter; a fresh
    session also sidesteps any session-affinity/expiry handling this module would
    otherwise need to get right.
    """
    base_url = os.environ.get("AGENT_BRAIN_RETAIN_BASE_URL", "").rstrip("/")
    if not base_url:
        raise AgentBrainNotConfiguredError("AGENT_BRAIN_RETAIN_BASE_URL is not set")
    headers = {"X-API-Key": os.environ.get("AGENT_BRAIN_RETAIN_API_KEY", "")}
    http_client = create_mcp_http_client(headers=headers)
    async with streamable_http_client(f"{base_url}/mcp", http_client=http_client) as (read, write):
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
