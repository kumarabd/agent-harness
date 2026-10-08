"""providers/ — the two pieces that are pure translation.

The provider classes themselves need API keys, but the shape conversions do not, and
they are where the two wire formats disagree most: OpenAI wraps tools in a
`{type: "function", function: {...}}` envelope that Anthropic does not have, and
Anthropic requires strict user/assistant alternation that OpenAI does not.
"""

import pytest

from tenant_worker.providers.anthropic_provider import (
    _openai_conversation_to_anthropic,
    _openai_tools_to_anthropic,
)
from tenant_worker.providers.base import parse_report_status


# ------------------------------------------------------------- report_status


def test_a_well_formed_report_status_is_parsed():
    parsed = parse_report_status(
        {"status": "blocked", "tier": "expert", "note": "waiting on the API", "est_remaining_steps": 3}
    )
    assert (parsed.status, parsed.tier, parsed.note, parsed.est_remaining_steps) == (
        "blocked", "expert", "waiting on the API", 3,
    )


def test_status_and_tier_are_normalised_before_matching():
    parsed = parse_report_status({"status": "  WORKING  ", "tier": "FAST"})
    assert parsed.status == "working" and parsed.tier == "fast"


@pytest.mark.parametrize(
    "args",
    [
        {"status": "nearly-done"},
        {"tier": "turbo"},
        {"status": None},
        "not a dict at all",
        None,
    ],
)
def test_unknown_or_malformed_values_are_dropped_rather_than_raised(args):
    # A weak model half-complying is common and must not fail the turn — an empty
    # status is the documented way of saying "it didn't really report".
    parsed = parse_report_status(args)
    assert parsed.status == "" and parsed.tier == ""


def test_a_negative_or_unparseable_step_count_becomes_zero():
    assert parse_report_status({"est_remaining_steps": -4}).est_remaining_steps == 0
    assert parse_report_status({"est_remaining_steps": "lots"}).est_remaining_steps == 0
    assert parse_report_status({"est_remaining_steps": None}).est_remaining_steps == 0


# ------------------------------------------------------------ tool translation


def test_tools_are_unwrapped_from_the_openai_envelope():
    tools = [
        {
            "type": "function",
            "function": {
                "name": "arm_wake",
                "description": "arm it",
                "parameters": {"type": "object", "properties": {"name": {"type": "string"}}},
            },
        }
    ]
    assert _openai_tools_to_anthropic(tools) == [
        {
            "name": "arm_wake",
            "description": "arm it",
            "input_schema": {"type": "object", "properties": {"name": {"type": "string"}}},
        }
    ]


def test_a_tool_with_no_parameters_still_gets_a_valid_schema():
    # Anthropic rejects a missing input_schema, so the fallback has to be a real
    # empty object schema and not None.
    result = _openai_tools_to_anthropic([{"type": "function", "function": {"name": "ping"}}])
    assert result[0]["input_schema"] == {"type": "object", "properties": {}}


# ------------------------------------------------------- conversation translation


def test_the_system_prompt_is_lifted_out_of_the_messages():
    system, messages = _openai_conversation_to_anthropic(
        [
            {"role": "system", "content": "be brief"},
            {"role": "system", "content": "be kind"},
            {"role": "user", "content": "hello"},
        ]
    )
    # Anthropic takes the system prompt as its own argument, not a message — and
    # several system messages have to survive as one.
    assert system == "be brief\n\nbe kind"
    assert messages == [{"role": "user", "content": "hello"}]


def test_assistant_tool_calls_become_tool_use_blocks_with_parsed_arguments():
    _, messages = _openai_conversation_to_anthropic(
        [
            {
                "role": "assistant",
                "content": "let me look",
                "tool_calls": [
                    {"id": "call_1", "type": "function", "function": {"name": "budget_status", "arguments": '{"limit": 3}'}}
                ],
            }
        ]
    )
    assert messages[0]["content"] == [
        {"type": "text", "text": "let me look"},
        # Arguments arrive as a JSON *string* (that is how they are stored in
        # Postgres); Anthropic wants the parsed object, not the string.
        {"type": "tool_use", "id": "call_1", "name": "budget_status", "input": {"limit": 3}},
    ]


def test_unparseable_tool_arguments_become_an_empty_object_rather_than_failing():
    _, messages = _openai_conversation_to_anthropic(
        [{"role": "assistant", "tool_calls": [{"id": "c", "function": {"name": "t", "arguments": "{not json"}}]}]
    )
    assert messages[0]["content"][0]["input"] == {}


def test_an_assistant_message_with_nothing_in_it_is_dropped():
    # An empty content array is rejected by the API, so it must not be sent.
    _, messages = _openai_conversation_to_anthropic(
        [{"role": "assistant", "content": ""}, {"role": "user", "content": "hi"}]
    )
    assert messages == [{"role": "user", "content": "hi"}]


def test_adjacent_tool_results_coalesce_into_one_user_turn():
    _, messages = _openai_conversation_to_anthropic(
        [
            {"role": "tool", "tool_call_id": "a", "content": "first"},
            {"role": "tool", "tool_call_id": "b", "content": "second"},
        ]
    )
    # Anthropic requires strict alternation, so two OpenAI tool messages — which
    # are siblings there — have to become one user turn with two result blocks.
    assert len(messages) == 1
    assert [b["tool_use_id"] for b in messages[0]["content"]] == ["a", "b"]


def test_a_tool_result_after_real_content_starts_its_own_turn():
    _, messages = _openai_conversation_to_anthropic(
        [
            {"role": "user", "content": "hello"},
            {"role": "tool", "tool_call_id": "a", "content": "first"},
        ]
    )
    # The preceding user turn is a plain string, not tool-result-only, so this must
    # NOT be appended to it.
    assert len(messages) == 2
    assert messages[1]["content"] == [{"type": "tool_result", "tool_use_id": "a", "content": "first"}]


def test_an_unrecognized_role_is_skipped_rather_than_corrupting_the_thread():
    _, messages = _openai_conversation_to_anthropic(
        [{"role": "developer", "content": "?"}, {"role": "user", "content": "hi"}]
    )
    assert messages == [{"role": "user", "content": "hi"}]
