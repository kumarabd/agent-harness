"""tools.py — the registry ToolCall dispatches through, and the session-directory
resolution every filesystem-touching tool depends on.

`TOOL_REGISTRY` is built at import from the capability table, with an `assert` as the
only guard against drift. These tests give that failure a name, and pin the timing
profiles that are otherwise invisible until a long tool call is killed early.
"""

import os

import pytest

from tenant_worker import tools
from tenant_worker.capabilities import HANDLER_REFS


def test_the_registry_covers_exactly_the_capabilities_that_have_handlers():
    assert set(tools.TOOL_REGISTRY) - {"search", "slow_tool", "noop_tool"} == set(HANDLER_REFS)


def test_every_registry_entry_points_at_the_handler_its_capability_named():
    for name, ref in HANDLER_REFS.items():
        assert tools.TOOL_REGISTRY[name].handler is tools._HANDLERS[ref]


def test_the_fixture_stubs_are_present_and_are_not_model_facing():
    # Scenario fixtures script calls to these by name; they are deliberately not in
    # the capability table, so the model is never offered them.
    for stub in ("search", "slow_tool", "noop_tool"):
        assert stub in tools.TOOL_REGISTRY
        assert stub not in HANDLER_REFS


def test_every_timing_profile_is_coherent():
    for name, spec in tools.TOOL_REGISTRY.items():
        # A heartbeat timeout at or below the interval means Temporal gives up
        # between beats, killing healthy long calls.
        assert spec.heartbeat_interval_seconds < spec.heartbeat_timeout_seconds, name
        assert 0 < spec.heartbeat_timeout_seconds <= spec.start_to_close_timeout_seconds, name


def test_heavier_tiers_are_given_longer_to_finish():
    # The tiers exist so a shell command is not cut off at a cognition-tier timeout.
    assert (
        tools.TOOL_REGISTRY["shell_exec"].start_to_close_timeout_seconds
        > tools.TOOL_REGISTRY["lcm_grep"].start_to_close_timeout_seconds
    )


def test_resolve_session_dir_joins_under_the_configured_root(monkeypatch):
    monkeypatch.setenv("SESSION_ROOT", "/srv/sessions")
    # fs_path arrives absolute ("/session/sess-1/"); joining it raw would escape the
    # root entirely, which is why the leading slash is stripped.
    assert tools.resolve_session_dir("/session/sess-1/") == "/srv/sessions/session/sess-1/"


def test_resolve_session_dir_falls_back_to_a_default_root(monkeypatch):
    monkeypatch.delenv("SESSION_ROOT", raising=False)
    resolved = tools.resolve_session_dir("/session/x/")
    assert resolved == os.path.join(tools._DEFAULT_SESSION_ROOT, "session/x/")


def test_a_tool_context_defaults_to_having_no_temporal_client():
    # Optional on purpose: a ToolCallActivity built without one still serves every
    # other tool, and only the wake tools complain — with their own clear message.
    assert "temporal_client" in {f.name for f in tools.ToolContext.__dataclass_fields__.values()}
    assert tools.ToolContext.__dataclass_fields__["temporal_client"].default is None
