"""Journaling domain policy. Only the user owns selection; chat owns dialogue."""

from __future__ import annotations

import json
import re

from .base import SkillDefinition, register

SYSTEM_PROMPT = """Advance the confirmed diary proposal using only your offered Notion tools.
The user has already approved the exact text. Preserve their words and uncertainty.
Find and fetch the unique page titled My Diary. If missing or ambiguous, stop and
explain the blocker; chat will ask the user. Search for the dated child beneath
that root using page_url and filters.title_only=true. Match the YYYY-MM-DD portion
even if an existing title has surrounding symbols. Never use an unscoped search
as evidence that a dated page is absent. Use the date specified in the proposal;
if none is provided, use the supplied current UTC date and state that choice.
Create at most one dated page (plain YYYY-MM-DD title, allow_async=false), or append
with command=insert_content, position.type=end, content=the exact approved text,
and allow_async=false to an existing page. Never replace existing content or
change properties, icons or covers. Read the Notion Markdown specification through
notion-fetch before writing. Use only operations present in the discovered schema.
After the write, fetch the SAME page and verify the exact entry is present.
Only verification ends this step successfully. Make one tool call per response.
You cannot select modes, ask the user directly, spawn agents, use shell, or deliver
a reply. Stop with an explanation if you need new information or authority."""

READ_TOOLS = frozenset({"notion-search", "notion-fetch"})
WRITE_TOOLS = frozenset({"notion-create-pages", "notion-update-page"})


def _strings(value):
    if isinstance(value, str):
        yield value
        try:
            parsed = json.loads(value)
        except (ValueError, TypeError):
            return
        if isinstance(parsed, (dict, list)):
            yield from _strings(parsed)
    elif isinstance(value, dict):
        for v in value.values():
            yield from _strings(v)
    elif isinstance(value, list):
        for v in value:
            yield from _strings(v)


def _refs(value) -> set[str]:
    return {
        m.replace("-", "").lower()
        for s in _strings(value)
        for m in re.findall(
            r"[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}",
            s,
        )
    }


def validate(tool: str, args: dict, content: str, history: list[dict]) -> None:
    if tool not in WRITE_TOOLS:
        return
    if any(h["request"].get("tool") in WRITE_TOOLS for h in history):
        raise ValueError(
            "one diary mutation per confirmed step; inspect the outcome before another write"
        )
    if tool == "notion-update-page":
        if args.get("command") != "insert_content" or args.get("position") != {
            "type": "end"
        }:
            raise ValueError("diary updates must append to the end, never replace")
        if set(args) - {"page_id", "command", "position", "content", "allow_async"}:
            raise ValueError(
                "diary append cannot change page metadata or other content"
            )
        written_text = args.get("content", "")
    else:
        pages = args.get("pages") or []
        if len(pages) != 1:
            raise ValueError("create exactly one dated diary page per confirmed entry")
        if set(args) - {"parent", "pages", "allow_async"} or set(pages[0]) - {
            "properties",
            "content",
        }:
            raise ValueError(
                "diary creation cannot apply templates or unrelated metadata"
            )
        properties = pages[0].get("properties") or {}
        if set(properties) != {"title"} or not re.fullmatch(
            r"\d{4}-\d{2}-\d{2}", str(properties["title"])
        ):
            raise ValueError("diary page title must be YYYY-MM-DD")
        if not (args.get("parent") or {}).get("page_id"):
            raise ValueError("diary page requires a parent page")
        written_text = pages[0].get("content", "")
    if args.get("allow_async") is not False:
        raise ValueError("diary mutations require allow_async=false")
    if not isinstance(written_text, str) or written_text.strip() != content.strip():
        raise ValueError("write must preserve the exact confirmed entry text")


def complete(content: str, current: dict, history: list[dict]) -> bool:
    if current["request"].get("tool") != "notion-fetch":
        return False
    response = current.get("response")
    if not response or response.get("isError") or response.get("error"):
        return False
    if content not in "\n".join(_strings(response)):
        return False
    args = current["request"].get("arguments", {})
    fetched = _refs(args.get("id", ""))
    return any(
        h["request"].get("tool") in WRITE_TOOLS
        and h.get("response") is not None
        and not h["response"].get("isError")
        and not h["response"].get("error")
        and bool(fetched & _written_pages(h))
        for h in history
    )


def _written_pages(observation: dict) -> set[str]:
    request = observation["request"]
    args = request.get("arguments", {})
    if request["tool"] == "notion-update-page":
        # Do not mistake UUIDs in entry text for the actual write target.
        return _refs(args.get("page_id", ""))
    # Creation has no target id in its request. Use the provider result,
    # excluding the parent/root reference, which is not the created page.
    return _refs(observation["response"]) - _refs(args.get("parent", {}))


register(
    SkillDefinition(
        name="journaling",
        description="Prepare, confirm, append and verify a diary entry in Notion.",
        system_prompt=SYSTEM_PROMPT,
        aliases=("journaling", "journal"),
        backend="notion",
        read_tools=READ_TOOLS,
        write_tools=WRITE_TOOLS,
        native_tools={},
        validate=validate,
        complete=complete,
    )
)
