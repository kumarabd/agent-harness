"""lcm/ — the pure pieces of context management.

The query paths need Postgres; the classification and text-assembly logic does not,
and that is where a wrong answer is quiet rather than loud: a transcript that drops
tool results still summarizes, just badly, and nobody finds out for a week.
"""

import uuid

import pytest

from tenant_worker.lcm.compaction import _build_transcript, compression_state
from tenant_worker.lcm.constants import estimate_tokens
from tenant_worker.lcm.retrieval import LCMNotFoundError, _parse_id


# ------------------------------------------------------------------ constants


def test_token_estimate_is_the_documented_quarter_length():
    assert estimate_tokens("a" * 400) == 100


def test_token_estimate_never_returns_zero():
    # A zero would let an empty message slip past every threshold check that
    # multiplies or compares against it.
    assert estimate_tokens("") == 1
    assert estimate_tokens(None) == 1
    assert estimate_tokens("ab") == 1


# ---------------------------------------------------------------- compaction


@pytest.mark.parametrize(
    "tokens,expected",
    [(0, "none"), (99, "none"), (100, "soft"), (199, "soft"), (200, "hard"), (10_000, "hard")],
)
def test_compression_state_is_a_two_tier_classification(tokens, expected):
    # The caller decides what soft (async) and hard (blocking) mean; this only names
    # which side of which line the turn is on.
    assert compression_state(tokens, soft_threshold=100, hard_threshold=200) == expected


def test_the_hard_threshold_wins_when_both_are_crossed():
    assert compression_state(250, soft_threshold=100, hard_threshold=200) == "hard"


def test_the_transcript_inlines_tool_results_beside_their_message():
    span = [
        {"message_id": "m1", "role": "user", "content": "what did I spend?"},
        {"message_id": "m2", "role": "assistant", "content": ""},
    ]
    by_message = {
        "m2": [
            {"tool_name": "spends_list", "status": "ok", "result": '{"items": []}'},
        ]
    }

    transcript = _build_transcript(span, by_message)

    # A tool result lives only in tool_calls.result and never in messages.content, so
    # a transcript that skipped it would summarize the turn as if nothing happened.
    assert transcript == (
        "user: what did I spend?\n"
        '  tool: spends_list -> {"items": []}'
    )


def test_a_failed_tool_call_is_labelled_rather_than_shown_as_its_result():
    span = [{"message_id": "m1", "role": "assistant", "content": "trying"}]
    by_message = {"m1": [{"tool_name": "shell_exec", "status": "error", "result": None}]}

    transcript = _build_transcript(span, by_message)

    # Without the status the summarizer would read a None result as an empty one.
    assert "(status=error)" in transcript


def test_a_message_with_no_content_and_no_tool_calls_contributes_nothing():
    assert _build_transcript([{"message_id": "m1", "role": "assistant", "content": ""}], {}) == ""


# ----------------------------------------------------------------- retrieval


def test_a_valid_id_parses():
    value = str(uuid.uuid4())
    assert str(_parse_id(value, "lcm_describe")) == value


@pytest.mark.parametrize("bad", ["not-a-uuid", "", None, 123])
def test_an_unparseable_id_names_the_tool_and_the_value(bad):
    with pytest.raises(LCMNotFoundError, match="lcm_expand"):
        _parse_id(bad, "lcm_expand")


def test_the_not_found_error_is_a_value_error_the_dispatcher_already_handles():
    # tool_call.py turns any handler exception into status='error' with str(exc), so
    # a plain ValueError subclass needs no special-casing at the dispatch layer.
    assert issubclass(LCMNotFoundError, ValueError)
