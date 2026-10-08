"""shell_hub.py — the pure half of the in-process command index.

The index itself needs the embedding service, but the filters around it do not, and
they are the difference between a useful index and a noisy one: a version banner
embeds just as close to an unrelated query as a real description does, because
neither contains enough meaning to separate them.
"""

import pytest

from tenant_worker import shell_hub


def test_a_real_description_is_substantial_enough_to_index():
    assert shell_hub._is_substantial("copy files and directories")
    assert shell_hub._is_substantial("Print lines matching a pattern")


@pytest.mark.parametrize(
    "line",
    [
        "GNU bash, version 5.2.37(1)-release",  # a version banner
        "help:",                                 # a stub
        "usage",
        "",                                      # nothing at all
    ],
)
def test_an_uninformative_line_is_filtered_out(line):
    assert not shell_hub._is_substantial(line)


def test_both_length_checks_have_to_pass():
    # Long but only one word: no real description.
    assert not shell_hub._is_substantial("averyveryverylongsingleword")
    # Three words but too short to say anything.
    assert not shell_hub._is_substantial("do a thing")


def test_version_banners_are_caught_case_insensitively_and_anywhere_in_the_line():
    assert not shell_hub._is_substantial("the tool, VERSION 1.2, does things")
    # A line merely containing "version" as prose is still allowed through.
    assert shell_hub._is_substantial("show the version history of a file")


def test_fts_queries_are_stripped_to_plain_lowercase_tokens():
    # Tantivy reads uppercase AND/OR/NOT as boolean operators, so a query that
    # happens to contain the word "and" must not become one.
    assert shell_hub._fts_safe("List AND Copy") == "list and copy"
    assert shell_hub._fts_safe("  weird! punctuation??  ") == "weird punctuation"
    assert shell_hub._fts_safe(None) == ""
    assert shell_hub._fts_safe("") == ""


def test_fts_queries_are_capped():
    capped = shell_hub._fts_safe(" ".join(f"w{n}" for n in range(100)))
    assert len(capped.split()) == shell_hub._FTS_MAX_TOKENS


@pytest.mark.asyncio
async def test_searching_before_init_returns_nothing_rather_than_raising():
    # The uninitialised case is reachable in any deployment without an embedding
    # endpoint configured, and tools.search_tools merges this result with mcp-hub's.
    assert shell_hub._collection is None
    assert await shell_hub.search("list files") == []
