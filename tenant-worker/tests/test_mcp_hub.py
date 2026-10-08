"""mcp_hub.py — the outbound client, both faces of it.

Two very different things live here. `call_tool` speaks MCP to mcp-hub's tool-broker
endpoint; the subscription functions speak plain HTTP to its management API. They are
kept separate because they are separate: MCP is the face the agent reaches through,
and `/api/subscriptions` is plumbing between the harness and the hub.

No sockets here. The subscription tests swap the module's `httpx` for a shim over
`MockTransport`, so real httpx still builds the URL and encodes the body — only the
transport is fake, which is the part that would otherwise need a network.
"""

from urllib.parse import unquote

import httpx
import pytest
from mcp import types

from tenant_worker import mcp_hub

HUB = "http://mcp-hub.test:8000"


class _ShimmedHttpx:
    """The `httpx` module as mcp_hub sees it, with the transport pre-bound."""

    HTTPError = httpx.HTTPError

    def __init__(self, handler):
        self._transport = httpx.MockTransport(handler)

    def AsyncClient(self, **kwargs):
        return httpx.AsyncClient(transport=self._transport, **kwargs)


@pytest.fixture
def hub(monkeypatch):
    """Returns (calls, respond). `respond` decides the response per request."""
    calls: list[httpx.Request] = []
    state = {"respond": lambda request: httpx.Response(200, json={})}

    def handler(request):
        calls.append(request)
        return state["respond"](request)

    monkeypatch.setenv("MCP_HUB_URL", HUB)
    monkeypatch.setattr(mcp_hub, "httpx", _ShimmedHttpx(handler))
    return calls, state


# --------------------------------------------------------------- base url


@pytest.mark.asyncio
async def test_an_unconfigured_hub_raises_before_any_call_is_attempted(monkeypatch):
    monkeypatch.delenv("MCP_HUB_URL", raising=False)
    # The distinct type matters: callers tolerate "not configured" (no watch can
    # exist either) but must not tolerate "the hub is down" the same way.
    with pytest.raises(mcp_hub.McpHubNotConfiguredError):
        await mcp_hub.list_subscriptions()
    with pytest.raises(mcp_hub.McpHubNotConfiguredError):
        await mcp_hub.arm_subscription("w", "k", "i", "o")


# ----------------------------------------------------------- subscriptions


@pytest.mark.asyncio
async def test_arming_posts_the_objective_with_the_subscription(hub):
    calls, state = hub
    state["respond"] = lambda request: httpx.Response(
        200, json={"event_key": "budget:8f14", "invoke_id": "u1", "wake_id": "watch:u1:dining"}
    )

    result = await mcp_hub.arm_subscription(
        wake_id="watch:u1:dining",
        event_key="budget:8f14",
        invoke_id="u1",
        objective="Tell them while there is room in the month.",
    )

    request = calls[0]
    assert request.method == "POST"
    assert str(request.url) == f"{HUB}/api/subscriptions"
    # The objective is sent ahead of time because the engine raising the notice
    # cannot know it — this assertion is the contract.
    assert request.content.decode() and "Tell them while there is room" in request.content.decode()
    assert result["wake_id"] == "watch:u1:dining"


@pytest.mark.asyncio
async def test_listing_returns_the_items_and_tolerates_an_empty_body(hub):
    calls, state = hub
    state["respond"] = lambda request: httpx.Response(
        200, json={"items": [{"wake_id": "watch:u1:dining", "event_key": "budget:8f14"}]}
    )
    assert await mcp_hub.list_subscriptions() == [
        {"wake_id": "watch:u1:dining", "event_key": "budget:8f14"}
    ]
    assert calls[0].method == "GET"

    state["respond"] = lambda request: httpx.Response(200, json={})
    assert await mcp_hub.list_subscriptions() == []


@pytest.mark.asyncio
async def test_revising_sends_only_what_it_was_given(hub):
    calls, state = hub
    state["respond"] = lambda request: httpx.Response(204)

    assert await mcp_hub.revise_subscription("watch:u1:dining", objective="reworded") is True
    body = calls[0].content.decode()
    # A field left out must be left out of the request, not sent empty — the hub
    # treats an omitted field as "leave alone" and an empty one as "clear".
    assert "objective" in body and "event_key" not in body
    assert calls[0].method == "PUT"


@pytest.mark.asyncio
async def test_revising_with_nothing_to_say_does_not_call_the_hub(hub):
    calls, _ = hub
    assert await mcp_hub.revise_subscription("watch:u1:dining") is False
    assert calls == []


@pytest.mark.asyncio
async def test_revising_something_that_was_never_armed_is_false_not_an_error(hub):
    _, state = hub
    state["respond"] = lambda request: httpx.Response(404, json={"error": "nope"})
    assert await mcp_hub.revise_subscription("watch:u1:nope", objective="x") is False


@pytest.mark.asyncio
async def test_cancelling_a_colon_bearing_id_survives_url_encoding(hub):
    calls, state = hub
    state["respond"] = lambda request: httpx.Response(204)

    assert await mcp_hub.cancel_subscription("watch:agent:main:user:u1:dining") is True
    # The whole id scheme is prefix:scope:name, so the colons have to survive.
    assert unquote(calls[0].url.path).endswith("/api/subscriptions/watch:agent:main:user:u1:dining")


@pytest.mark.asyncio
async def test_cancelling_something_that_was_never_armed_is_false(hub):
    _, state = hub
    state["respond"] = lambda request: httpx.Response(404)
    assert await mcp_hub.cancel_subscription("watch:u1:nope") is False


@pytest.mark.asyncio
async def test_an_unreachable_hub_raises_this_modules_own_error(hub):
    def explode(request):
        raise httpx.ConnectError("connection refused", request=request)

    _, state = hub
    state["respond"] = explode

    # Raw httpx exceptions escaping here would mean every caller has to know about
    # them — and the ones merely tolerating "unconfigured" would fail outright on
    # "briefly unreachable" instead.
    for call in (
        lambda: mcp_hub.list_subscriptions(),
        lambda: mcp_hub.arm_subscription("w", "k", "i", "o"),
        lambda: mcp_hub.revise_subscription("w", objective="x"),
        lambda: mcp_hub.cancel_subscription("w"),
    ):
        with pytest.raises(mcp_hub.McpHubCallError):
            await call()


@pytest.mark.asyncio
async def test_a_server_error_is_reported_with_its_status(hub):
    _, state = hub
    state["respond"] = lambda request: httpx.Response(500, text="boom")
    with pytest.raises(mcp_hub.McpHubCallError, match="500"):
        await mcp_hub.arm_subscription("w", "k", "i", "o")


# --------------------------------------------------------------- call_tool


class _FakeTransport:
    async def __aenter__(self):
        return (None, None)

    async def __aexit__(self, *exc):
        return False


def _install_mcp(monkeypatch, result):
    class FakeSession:
        async def __aenter__(self):
            return self

        async def __aexit__(self, *exc):
            return False

        async def initialize(self):
            return None

        async def call_tool(self, name, arguments):
            return result

    monkeypatch.setenv("MCP_HUB_URL", HUB)
    monkeypatch.setattr(mcp_hub, "streamable_http_client", lambda url: _FakeTransport())
    monkeypatch.setattr(mcp_hub, "ClientSession", lambda read, write: FakeSession())


@pytest.mark.asyncio
async def test_call_tool_prefers_structured_content(monkeypatch):
    _install_mcp(
        monkeypatch,
        types.CallToolResult(content=[], structured_content={"items": [1]}),
    )
    assert await mcp_hub.call_tool("search_tools", {"query": "x"}) == {"items": [1]}


@pytest.mark.asyncio
async def test_call_tool_falls_back_to_parsing_a_text_block(monkeypatch):
    _install_mcp(
        monkeypatch,
        types.CallToolResult(content=[types.TextContent(type="text", text='{"items": [2]}')]),
    )
    assert await mcp_hub.call_tool("search_tools", {"query": "x"}) == {"items": [2]}


@pytest.mark.asyncio
async def test_call_tool_raises_on_an_error_result(monkeypatch):
    _install_mcp(
        monkeypatch,
        types.CallToolResult(content=[types.TextContent(type="text", text="backend exploded")], is_error=True),
    )
    # Surfaced, not returned as data: a tool that failed is not a tool that answered.
    with pytest.raises(mcp_hub.McpHubCallError, match="backend exploded"):
        await mcp_hub.call_tool("search_tools", {"query": "x"})


@pytest.mark.asyncio
async def test_call_tool_raises_on_a_result_with_no_content(monkeypatch):
    _install_mcp(monkeypatch, types.CallToolResult(content=[]))
    with pytest.raises(mcp_hub.McpHubCallError, match="no content"):
        await mcp_hub.call_tool("search_tools", {"query": "x"})
