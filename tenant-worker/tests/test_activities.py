"""The pure helpers that live inside activity modules.

These modules are mostly `@activity.defn` methods that need a pool, but each carries
one or two functions with no dependencies at all — and those are the ones deciding
what the user actually reads when a turn is stopped, or how a subagent's context is
seeded.
"""

import pytest

from tenant_worker.model_call import (
    _WRAP_UP_REASONS,
    _resolve_gating,
    _validate_subagent_delegation,
    _wrap_up_instruction,
)
from tenant_worker.seed_child_session import _render_entry
from tenant_worker.status_ping import _describe_call


# ------------------------------------------------------------------ status_ping


def test_a_call_is_described_by_its_most_meaningful_argument():
    # Generic rather than a per-tool switch, so a new tool using one of these
    # key names is covered for free.
    assert _describe_call("shell_exec", {"command": "ls -la"}) == 'shell_exec — "ls -la"'
    assert _describe_call("recall", {"query": "what did I say"}) == 'recall — "what did I say"'
    assert _describe_call("lcm_grep", {"pattern": "x", "content": "body"}) == 'lcm_grep — "body"'


def test_an_empty_or_missing_argument_falls_through_to_the_bare_name():
    assert _describe_call("recall", {"query": "   "}) == "recall"
    assert _describe_call("report_status", {"status": "working"}) == "report_status"
    assert _describe_call("noop_tool", {}) == "noop_tool"


def test_a_long_argument_is_truncated_to_keep_the_ping_small():
    described = _describe_call("shell_exec", {"command": "x" * 200})
    assert described.endswith('..."')
    assert len(described) < 100


# ------------------------------------------------------------ seed_child_session


def test_a_seeded_entry_names_its_role_and_content():
    assert _render_entry({"role": "user", "content": "hello"}) == "user: hello"


def test_a_seeded_entry_lists_the_tools_that_message_called():
    rendered = _render_entry(
        {
            "role": "assistant",
            "content": "looking",
            "tool_calls": [{"function": {"name": "budget_status"}}, {"function": {"name": "spends_list"}}],
        }
    )
    # Without the names, a subagent inherits an assistant turn that appears to have
    # answered out of nowhere.
    assert rendered == "assistant: looking [called: budget_status, spends_list]"


def test_a_tool_result_is_rendered_distinctly():
    assert _render_entry({"role": "tool", "content": "[]"}) == "tool result: []"


def test_missing_content_does_not_produce_a_literal_none():
    assert _render_entry({"role": "user"}) == "user: "
    assert _render_entry({}) == "unknown: "


# ------------------------------------------------------------------- model_call


def test_the_wrap_up_instruction_names_the_tool_calls_as_stopped():
    instruction = _wrap_up_instruction("budget_exhausted")
    assert "this turn used up its token budget" in instruction
    # The instruction has to forbid more tools, or the model replies by calling one.
    assert "Do not call any more tools" in instruction
    assert "what they can try next" in instruction


def test_an_unrecognised_reason_is_still_rendered_as_something_readable():
    # The reason is interpolated verbatim when it has no friendly sentence, since a
    # turn being stopped is not the moment to raise a KeyError.
    assert "mystery_reason" in _wrap_up_instruction("mystery_reason")


def test_every_declared_wrap_up_reason_has_a_sentence():
    for reason in _WRAP_UP_REASONS:
        assert _WRAP_UP_REASONS[reason]
        assert _WRAP_UP_REASONS[reason] in _wrap_up_instruction(reason)


def test_a_nested_subagent_must_state_what_it_delegates_and_what_it_keeps():
    assert _validate_subagent_delegation({"delegated_scope": "the migration", "kept_work": "the review"}) is None

    rejected = _validate_subagent_delegation({"delegated_scope": "the migration"})
    assert rejected is not None
    assert "kept_work" in rejected

    # Blank strings count as absent — the paper's failure mode is an inability to
    # state anything, so whitespace is not a statement.
    assert _validate_subagent_delegation({"delegated_scope": "  ", "kept_work": "  "}) is not None


@pytest.mark.parametrize("tool_name", ["recall", "discover_tools", "report_status", "lcm_grep"])
def test_non_side_effecting_tools_are_never_gated(tool_name):
    assert _resolve_gating(tool_name, {}) == (False, "", "")


def test_an_empty_command_cannot_be_gated():
    assert _resolve_gating("shell_exec", {}) == (False, "", "")
    assert _resolve_gating("shell_exec", {"command": "   "}) == (False, "", "")
