"""claim_check.py — the route a large tool result takes instead of into the context.

This sits on the boundary between "the model sees the output" and "the model sees a
pointer to the output", so the failure modes are silent: a preview that drops the tail
hides the error message the model needed, and a path the model cannot `cat` is a
reference to nothing.
"""

from pathlib import Path

import pytest

from tenant_worker import claim_check
from tenant_worker.claim_check import SMALL_OUTPUT_BYTES, is_claim_check_dir, store_if_large, store_result_if_large


def test_an_id_becomes_a_filename_that_is_safe_to_quote():
    # ":act:" is legal on POSIX but a footgun for anything shell-quoting the path
    # later — and the model is told to shell out to this file.
    assert claim_check._sanitize_id("sess-1:turn:2:act:3") == "sess-1_turn_2_act_3"


def test_a_short_payload_needs_no_tail():
    head, tail = claim_check._preview("short")
    assert (head, tail) == ("short", "")


def test_a_long_payload_is_split_head_and_tail_without_overlap():
    text = "H" * claim_check.HEAD_BYTES + "M" * 100 + "T" * claim_check.TAIL_BYTES
    head, tail = claim_check._preview(text)

    # Head+tail rather than head-only: a runaway command's diagnostic is at the END,
    # which head-only truncation silently dropped.
    assert head == "H" * claim_check.HEAD_BYTES
    assert tail.endswith("T" * claim_check.TAIL_BYTES)
    assert len(head.encode()) + len(tail.encode()) <= SMALL_OUTPUT_BYTES


def test_the_claim_check_directory_is_recognisable_for_pruning():
    assert is_claim_check_dir(".claim-check")
    assert not is_claim_check_dir("notes")


# --------------------------------------------------------------- store_if_large


@pytest.mark.asyncio
async def test_a_small_result_is_returned_inline_and_writes_nothing(tmp_path):
    result = await store_if_large(str(tmp_path), "tc:1", "result", b"small output")

    assert result == {"inline": "small output"}
    assert list(tmp_path.iterdir()) == []


@pytest.mark.asyncio
async def test_a_large_result_is_written_and_referenced_by_a_relative_path(tmp_path):
    data = b"x" * (SMALL_OUTPUT_BYTES + 1)

    result = await store_if_large(str(tmp_path), "sess-1:turn:2:act:3", "result", data)

    # Relative to the session directory, because that is what the model can pass to
    # a subsequent shell_exec — an absolute path would name a file it cannot see.
    assert result["claim_check_path"] == ".claim-check/sess-1_turn_2_act_3.result.log"
    assert result["size_bytes"] == len(data)
    assert result["exploration_summary"]["type"]
    assert "shell_exec" in result["note"]

    written = tmp_path / result["claim_check_path"]
    assert written.read_bytes() == data


@pytest.mark.asyncio
async def test_the_preview_of_a_large_result_carries_both_ends(tmp_path):
    data = b"H" * 3000 + b"M" * 5000 + b"T" * 3000

    result = await store_if_large(str(tmp_path), "tc:1", "result", data)

    assert result["head"].startswith("HHH")
    assert result["tail"].endswith("TTT")
    assert "MMM" not in result["head"] + result["tail"]


@pytest.mark.asyncio
async def test_the_threshold_is_inclusive(tmp_path):
    exactly = b"y" * SMALL_OUTPUT_BYTES
    result = await store_if_large(str(tmp_path), "tc:1", "result", exactly)
    # One byte over is what routes to disk; at the limit it still fits inline.
    assert "inline" in result


@pytest.mark.asyncio
async def test_undecodable_bytes_do_not_fail_the_write(tmp_path):
    data = b"\xff\xfe" + b"z" * SMALL_OUTPUT_BYTES

    result = await store_if_large(str(tmp_path), "tc:1", "result", data)

    # A large binary result still has to be storable — only the preview degrades.
    assert result["size_bytes"] == len(data)
    assert Path(tmp_path, result["claim_check_path"]).exists()


@pytest.mark.asyncio
async def test_storing_a_result_json_encodes_it_first(tmp_path):
    result = await store_result_if_large(str(tmp_path), "tc:1", {"items": list(range(2000))})
    assert result["claim_check_path"].endswith(".result.log")
    assert b"items" in Path(tmp_path, result["claim_check_path"]).read_bytes()
