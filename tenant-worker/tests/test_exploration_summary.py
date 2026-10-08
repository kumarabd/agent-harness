"""exploration_summary.py — what the model is told about output it cannot see.

Everything here is deterministic when no provider is configured, which is both the
fixture path and the degraded path. The shape description is the whole value: a
claim-check reference without one is a filename, and the model has to spend a
shell call to find out whether it mattered.
"""

import json

import pytest

from tenant_worker import exploration_summary as es


def test_binary_is_detected_by_a_nul_byte():
    # The cheap filter that keeps real binaries out of the json/csv parsers.
    assert es._is_binary(b"PK\x03\x04\x00\x00binary")
    assert not es._is_binary("plain text, even with a \x07 bell".encode())


def test_type_names_cover_the_json_value_space():
    assert es._type_name(None) == "null"
    assert es._type_name(True) == "bool"
    assert es._type_name(3) == "int"
    assert es._type_name(3.5) == "float"
    assert es._type_name("s") == "string"
    assert es._type_name([]) == "array"
    assert es._type_name({}) == "object"
    # bool must not fall through to int — it is a subclass of it in Python.
    assert es._type_name(False) != "int"


def test_json_shape_describes_keys_and_value_types():
    summary = es._try_json('{"name": "x", "count": 2, "tags": ["a"], "nested": {"k": true}}')

    assert summary["type"] == "json"
    assert summary["size_bytes"] == len('{"name": "x", "count": 2, "tags": ["a"], "nested": {"k": true}}')
    shape = summary["shape"]
    assert shape["kind"] == "object"
    assert shape["keys"] == ["name", "count", "tags", "nested"]
    assert shape["value_types"] == {
        "name": "string", "count": "int", "tags": "array", "nested": "object",
    }


def test_an_array_becomes_a_length_and_an_item_shape():
    shape = es._try_json("[1, 2, 3]")["shape"]
    assert shape["kind"] == "array"
    assert shape["length"] == 3
    assert shape["item_shape"]["type"] == "int"


def test_something_that_is_not_json_falls_through_rather_than_raising():
    # Returning None is the contract that lets the caller try CSV and then text.
    assert es._try_json("this is not json {") is None
    assert es._try_json("") is None


def test_an_unparseable_utf8_byte_in_the_text_does_not_raise():
    assert es._try_json("nope") is None


def test_csv_is_detected_with_its_header_and_row_count():
    summary = es._try_csv("name,count\nalpha,1\nbeta,2\n")
    assert summary["type"] == "csv"
    assert summary["delimiter"] == ","
    assert summary["columns"] == ["name", "count"]
    assert summary["row_count"] == 2
    assert summary["sample_rows"] == [["alpha", "1"], ["beta", "2"]]


def test_a_tab_separated_file_is_still_csv():
    # Sniffer detects the dialect rather than assuming a comma.
    assert es._try_csv("a\tb\nc\td\n")["type"] == "csv"


@pytest.mark.parametrize("text", ["just one line of prose", "one_column\nvalue_one\nvalue_two\n", ""])
def test_text_that_only_looks_like_csv_falls_through(text):
    assert es._try_csv(text) is None


def test_prose_that_sniffs_a_letter_as_its_delimiter_is_not_csv():
    # Found by this test: csv.Sniffer nominates "u" for this input, which splits each
    # line into two "columns" and so satisfied the two-column check. Left unfixed it
    # told the model it was looking at a 2-column CSV named ["single_col", "mn"].
    assert es._try_csv("single_column\nvalue_one\nvalue_two\n") is None


@pytest.mark.parametrize("delimiter", [",", "\t", ";", "|"])
def test_every_real_delimiter_still_sniffs(delimiter):
    text = delimiter.join(["a", "b"]) + "\n" + delimiter.join(["1", "2"]) + "\n"
    assert es._try_csv(text)["delimiter"] == delimiter


@pytest.mark.asyncio
async def test_summarize_degrades_deterministically_without_a_provider():
    # The fixture path and the unconfigured path are the same path: no LLM round
    # trip, and never a raise.
    for payload, expected in [
        (json.dumps({"a": 1}).encode(), "json"),
        (b"a,b\n1,2\n", "csv"),
        (b"PK\x03\x04\x00\x00", "binary"),
    ]:
        assert (await es.summarize(payload, provider=None))["type"] == expected


@pytest.mark.asyncio
async def test_summarize_returns_a_summary_for_plain_prose_too():
    summary = await es.summarize(b"Just some prose that is not structured at all.", provider=None)
    assert summary["type"]
    assert summary["size_bytes"] == len(b"Just some prose that is not structured at all.")


@pytest.mark.asyncio
async def test_summarize_never_raises_on_an_empty_payload():
    # claim_check calls this unconditionally and does not defend against failure.
    assert (await es.summarize(b"", provider=None))["type"]
