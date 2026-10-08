"""sentence_segmenter.py — when to flush a streaming chunk to delivery.

The whole point of this module is that it is *not* naive period-splitting: "Dr. Smith
said..." is one sentence. A regression here is not an exception, it is speech that
pauses in the middle of a name.
"""

import pytest

from tenant_worker.sentence_segmenter import find_boundary


def test_finds_a_boundary_just_past_the_whitespace():
    buffer = "Hello there. Next one"
    # The returned index includes the trailing space, because the caller flushes
    # buffer[:index] and keeps the rest — leaving the space would start the next
    # chunk with a stray indent.
    assert find_boundary(buffer) == len("Hello there. ")


def test_returns_none_until_a_sentence_actually_ends():
    assert find_boundary("no punctuation here at all") is None
    assert find_boundary("") is None
    assert find_boundary("trailing space but no full stop ") is None


@pytest.mark.parametrize(
    "buffer",
    [
        "Ask Dr. Smith about it. Then continue",   # abbreviation
        "It costs approx. 5 dollars. Then continue",  # abbreviation not before a name
        "Written by J. K. Rowling, apparently. Next",  # single-letter initials
    ],
)
def test_abbreviations_and_initials_do_not_end_a_sentence(buffer):
    boundary = find_boundary(buffer)
    # The boundary found must be the REAL one at the end, never the abbreviation.
    assert boundary is not None
    assert buffer[:boundary].rstrip().endswith(("it.", "dollars.", "apparently."))


@pytest.mark.parametrize(
    "buffer,expected",
    [
        ("Pi is 3.14 exactly. Next", len("Pi is 3.14 exactly. ")),   # no space after the dot
        ("See U.S. policy. Next", len("See U.S. policy. ")),         # multi-period abbreviation
    ],
)
def test_decimals_and_multi_period_abbreviations_are_not_boundaries(buffer, expected):
    # Excluded by the whitespace requirement alone — no separate digit check.
    assert find_boundary(buffer) == expected


def test_multiple_punctuation_and_a_closing_quote_both_end_a_sentence():
    assert find_boundary("Wow!! Really now") == len("Wow!! ")
    assert find_boundary('He said "stop." Then') == len('He said "stop." ')


def test_the_first_boundary_wins():
    buffer = "One. Two. Three"
    assert find_boundary(buffer) == len("One. ")


def test_an_abbreviation_does_not_stop_the_search_for_a_later_boundary():
    # The subtle failure: skipping the abbreviation by returning None instead of
    # continuing would stall the stream until more text arrived.
    buffer = "Dr. Smith arrived. More text follows"
    assert find_boundary(buffer) == len("Dr. Smith arrived. ")
