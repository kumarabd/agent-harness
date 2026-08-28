"""Type-aware Exploration Summary — docs/components/context-slot.md,
"Resolved: Duties and Strategies" duty #2 (large content: never loaded
into context directly, represented by a reference PLUS a type-aware
Exploration Summary — schema/shape extraction for structured data,
structural analysis for code, LLM summary for unstructured text, LCM
§2.2).

Sits on top of claim_check.py — the storage layer that landed 2026-08-27.
That layer already writes large tool outputs to the PV and returns a
reference; this module produces the type-appropriate description of
what's IN that reference, so the model sees something more useful than
just head/tail bytes when the output is structured (JSON, CSV) or has
otherwise-parseable shape.

Design principles, all deliberate:

1. **Content-type detection is cheap, deterministic, and comes first.**
   No LLM call to decide "what kind of data is this" — try `json.loads`,
   try `csv.Sniffer`, fall through to unstructured text. Detection failure
   never raises; the summarizer degrades to head/tail (what
   claim_check.py already returns) rather than failing the tool call.

2. **LLM summarization is the last resort, not the default.** Structured
   data (JSON, CSV) gets a deterministic shape/schema description with
   zero model round-trips — cheaper, faster, and more useful (a schema
   is a fact; an LLM paraphrase is a guess). Code was originally listed
   in the LCM §2.2 taxonomy as its own category (tree-sitter-shaped AST
   walk) but isn't detected here yet — a real code-structural analyzer
   is a genuine dependency (tree-sitter or per-language parsers) and
   shell_exec's output today doesn't reliably look like a source file to
   distinguish it from any other text blob without more signal (e.g. the
   command that produced it). Deferred pending real usage evidence,
   consistent with this project's numeric-tuning discipline elsewhere;
   text-tier LLM summary handles the code case adequately in the
   meantime, if less efficiently.

3. **LLM path is skippable if the model or client isn't configured.**
   Same graceful-degradation pattern agent_brain.py/mcp_hub.py already
   use — an unconfigured environment returns the deterministic-only
   summary (still better than raw head/tail alone for structured data).

4. **Cost gating: only unstructured text pays for an LLM call.** JSON
   and CSV never invoke the model — their summaries are a parse. The
   LLM call is a real per-large-output cost, so it's confined to the
   one content type where it genuinely earns its keep.

5. **Bounded output.** Summary contents are capped so a pathologically
   nested JSON blob or an enormous CSV header can't itself become the
   thing pushing context over the limit — the whole point of this
   module is to bound what the model sees, not to move where the
   unboundedness lives.
"""

from __future__ import annotations

import csv
import io
import json
import logging
from typing import Any

logger = logging.getLogger(__name__)

# Caps on individual summary fields — the summary is supposed to describe
# the shape of large content, not embed a second copy of it. All chosen
# small deliberately; a caller wanting the raw bytes reads
# claim_check_path instead. Same numeric-tuning discipline as everything
# else in this project — revisit once real usage data exists.
_MAX_JSON_KEYS = 32
_MAX_JSON_SAMPLE_ROWS = 3
_MAX_CSV_COLUMNS = 32
_MAX_CSV_SAMPLE_ROWS = 3
_MAX_TEXT_LLM_INPUT_BYTES = 32 * 1024  # cap on how much of a large text blob we ask the model to summarize
_MAX_LLM_SUMMARY_BYTES = 1024  # cap on the LLM's returned summary itself


async def summarize(data: bytes, provider=None, model: str = "") -> dict:
    """Produces a type-aware Exploration Summary for `data`. Never raises
    on malformed content — degrades to `{"type": "unstructured", ...}`
    (with or without LLM enrichment depending on provider availability)
    so the caller's tool call keeps succeeding.

    Returned shape always includes `type` (one of "json", "csv", "text",
    "binary") plus type-specific fields. See per-branch docstrings for
    the actual fields.
    """
    if _is_binary(data):
        return {"type": "binary", "size_bytes": len(data)}

    try:
        text = data.decode("utf-8", errors="strict")
    except UnicodeDecodeError:
        # Mostly-text-but-has-invalid-bytes — treat as text with
        # replacement, better than dropping to binary and losing what
        # structure the readable portion has.
        text = data.decode("utf-8", errors="replace")

    json_summary = _try_json(text)
    if json_summary is not None:
        return json_summary

    csv_summary = _try_csv(text)
    if csv_summary is not None:
        return csv_summary

    return await _summarize_text(text, provider, model)


def _is_binary(data: bytes) -> bool:
    """Cheap heuristic: NUL bytes in the first few KB. Real binary files
    (elf, images, archives) hit this fast; text with the odd control
    character doesn't. Not a perfect classifier, just a filter to keep
    genuine binaries from being fed into json/csv parsers that would
    error out on them harmlessly but waste the effort."""
    return b"\x00" in data[:8192]


def _try_json(text: str) -> dict | None:
    """JSON handler — schema/shape extraction, no LLM. Returns None if
    the text isn't parseable as JSON, so the caller can fall through."""
    try:
        obj = json.loads(text)
    except (json.JSONDecodeError, ValueError):
        return None

    return {
        "type": "json",
        "size_bytes": len(text.encode("utf-8")),
        "shape": _describe_json(obj),
    }


def _describe_json(obj: Any, depth: int = 0) -> dict:
    """Recursive shape description — a single JSON object becomes a
    schema (keys + value-type-per-key), a JSON array becomes
    {kind: array, length, item_shape}. Depth-limited (nested inspection
    stops at depth 3) so a deeply-nested blob can't produce an
    unbounded summary.
    """
    if isinstance(obj, dict):
        keys = list(obj.keys())
        truncated = len(keys) > _MAX_JSON_KEYS
        shown_keys = keys[:_MAX_JSON_KEYS]
        result: dict = {
            "kind": "object",
            "key_count": len(keys),
            "keys": shown_keys,
        }
        if truncated:
            result["keys_truncated"] = True
        if depth < 3:
            result["value_types"] = {k: _type_name(obj[k]) for k in shown_keys}
        return result
    if isinstance(obj, list):
        result = {"kind": "array", "length": len(obj)}
        if obj and depth < 3:
            # Sample from the first element only — describing every
            # element's shape independently could explode on a mixed-type
            # array. First-element inference is what tools like `jq` and
            # every JSON schema generator do too.
            result["item_shape"] = _describe_json(obj[0], depth + 1)
            if len(obj) > 1:
                result["sample"] = [_type_name(x) for x in obj[: _MAX_JSON_SAMPLE_ROWS]]
        return result
    return {"kind": "scalar", "type": _type_name(obj)}


def _type_name(obj: Any) -> str:
    if obj is None:
        return "null"
    if isinstance(obj, bool):
        return "bool"
    if isinstance(obj, int):
        return "int"
    if isinstance(obj, float):
        return "float"
    if isinstance(obj, str):
        return "string"
    if isinstance(obj, list):
        return "array"
    if isinstance(obj, dict):
        return "object"
    return type(obj).__name__


def _try_csv(text: str) -> dict | None:
    """CSV handler — header + row count + a few sample rows, no LLM.
    Uses csv.Sniffer to detect the actual dialect (comma, tab, semicolon)
    rather than assuming comma. Returns None if the text doesn't look
    like CSV, so the caller can fall through to the text summarizer.

    "Looks like CSV" is a real judgment call — csv.Sniffer succeeds on
    genuinely arbitrary text if you're unlucky, so we add a couple of
    disqualifying checks: at least 2 rows, header row not just one
    column (a single column is more likely just plain text).
    """
    # Only sample from the start for sniffing — Sniffer's own default
    # sample is bounded but slow; capping ourselves keeps the CSV probe
    # cheap even against a multi-MB input.
    sample = text[:4096]
    try:
        dialect = csv.Sniffer().sniff(sample)
    except csv.Error:
        return None

    try:
        reader = csv.reader(io.StringIO(text), dialect=dialect)
        rows = list(reader)
    except csv.Error:
        return None

    if len(rows) < 2:
        return None
    header = rows[0]
    if len(header) < 2:
        return None

    truncated_cols = len(header) > _MAX_CSV_COLUMNS
    sample_rows = rows[1 : 1 + _MAX_CSV_SAMPLE_ROWS]
    return {
        "type": "csv",
        "size_bytes": len(text.encode("utf-8")),
        "delimiter": dialect.delimiter,
        "row_count": len(rows) - 1,  # -1 for the header
        "column_count": len(header),
        "columns": header[:_MAX_CSV_COLUMNS],
        "columns_truncated": truncated_cols,
        "sample_rows": [row[:_MAX_CSV_COLUMNS] for row in sample_rows],
    }


async def _summarize_text(text: str, provider, model: str) -> dict:
    """Unstructured-text handler. Deterministic metrics always
    (line/word/byte counts — useful even without an LLM). If a Provider
    + model is available, also asks the model for a short natural-
    language summary; degrades gracefully if not configured or if the
    call fails.

    Never raises — a network/API failure logs a warning and returns the
    deterministic-only summary. The tool call itself must keep
    succeeding regardless of an external service's availability, same
    graceful-degradation pattern agent_brain.py already uses for
    memory_search failures."""
    lines = text.splitlines()
    result: dict = {
        "type": "text",
        "size_bytes": len(text.encode("utf-8")),
        "line_count": len(lines),
        "word_count": len(text.split()),
    }

    if provider is None or not model:
        return result

    prompt_input = text[:_MAX_TEXT_LLM_INPUT_BYTES]
    truncated_for_llm = len(text) > _MAX_TEXT_LLM_INPUT_BYTES
    try:
        response = await provider.summarize_text(
            system_prompt=(
                "Summarize this text in 2-4 short sentences, focusing on what kind of "
                "content it is (log output, prose, config file, etc.) and any obvious "
                "structure or errors. Be concrete; don't paraphrase content the caller "
                "can already see."
            ),
            user_content=prompt_input,
            model=model,
        )
        summary = response.content.strip()
    except Exception:  # noqa: BLE001 - real network/API failure, bounded then degrade
        logger.warning("exploration_summary: LLM summarize failed, returning deterministic-only", exc_info=True)
        return result

    if len(summary.encode("utf-8")) > _MAX_LLM_SUMMARY_BYTES:
        summary = summary.encode("utf-8")[:_MAX_LLM_SUMMARY_BYTES].decode("utf-8", errors="replace") + "…"
    result["summary"] = summary
    if truncated_for_llm:
        result["summary_from_prefix_only"] = True
    return result
