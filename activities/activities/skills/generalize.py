"""The generalization pass — one medium-tier model call that turns
successful task trajectories (plus any matched failures, plus a current body
for refinement) into a structured procedure
(docs/components/skill-subsystem.md, "The generalization pass").

Returns `None` when the model tier is unconfigured or the output can't be
parsed — the caller then skips that synthesis group rather than writing a
malformed procedure.
"""

from __future__ import annotations

import json
import logging
import re

from .. import llm_client, model_registry

logger = logging.getLogger(__name__)

_MAX_TOKENS = 1100
_MAX_TRANSCRIPTS = 5
_MAX_TRANSCRIPT_CHARS = 6_000

_SYSTEM_PROMPT = (
    "You reconstruct a reusable procedure from transcripts of a task the agent completed "
    "SUCCESSFULLY. Output ONLY a JSON object — no prose, no markdown fences — with exactly:\n"
    '{\n'
    '  "title": "short label",\n'
    '  "trigger_text": "one sentence: when this procedure applies — general, not tied to the specific instance",\n'
    '  "body": [ { "step_id": "short-slug", "instruction": "what to do", '
    '"tool_ref": "abstract tool description or null", "slots": ["names"] } ],\n'
    '  "preconditions": ["strings"],\n'
    '  "done_criteria": ["strings"],\n'
    '  "notes": ["cautions — especially anything a failed attempt or a mid-task correction revealed"]\n'
    "}\n"
    "Reconstruct the EFFECTIVE procedure the successful runs actually followed — the plan as amended "
    "by any correction visible in the transcript. Keep every tool_ref ABSTRACT (\"a version-control "
    "tool\"), never a concrete tool name. Weight recent transcripts more. If a CURRENT PROCEDURE is "
    "given, improve it in place rather than starting over."
)


def _clip(text: str) -> str:
    return text if len(text) <= _MAX_TRANSCRIPT_CHARS else text[:_MAX_TRANSCRIPT_CHARS] + "\n…(truncated)"


def _extract_json(raw: str) -> dict | None:
    text = (raw or "").strip()
    if text.startswith("```"):
        text = re.sub(r"^```[a-zA-Z0-9]*\n?", "", text)
        text = re.sub(r"\n?```$", "", text).strip()
    for candidate in (text, text[text.find("{") : text.rfind("}") + 1] if "{" in text else ""):
        try:
            obj = json.loads(candidate)
            if isinstance(obj, dict):
                return obj
        except (json.JSONDecodeError, ValueError):
            continue
    return None


def _valid(spec: dict) -> bool:
    if not isinstance(spec.get("title"), str) or not spec["title"].strip():
        return False
    if not isinstance(spec.get("trigger_text"), str) or not spec["trigger_text"].strip():
        return False
    body = spec.get("body")
    return isinstance(body, list) and len(body) >= 1 and all(
        isinstance(s, dict) and isinstance(s.get("instruction"), str) and s["instruction"].strip() for s in body
    )


def _normalize(spec: dict) -> dict:
    steps = []
    for i, s in enumerate(spec["body"], start=1):
        tool = s.get("tool_ref")
        steps.append(
            {
                "step_id": (s.get("step_id") or f"step-{i}").strip()[:40],
                "instruction": s["instruction"].strip(),
                "tool_ref": tool.strip() if isinstance(tool, str) and tool.strip() else None,
                "slots": [x.strip() for x in s.get("slots", []) if isinstance(x, str) and x.strip()],
            }
        )
    strs = lambda key: [x.strip() for x in spec.get(key, []) if isinstance(x, str) and x.strip()]  # noqa: E731
    return {
        "title": spec["title"].strip()[:120],
        "trigger_text": spec["trigger_text"].strip()[:400],
        "body": steps,
        "preconditions": strs("preconditions"),
        "done_criteria": strs("done_criteria"),
        "notes": strs("notes"),
    }


async def generalize(
    success_transcripts: list[str],
    failure_transcripts: list[str],
    current_body_text: str | None,
) -> dict | None:
    if not success_transcripts:
        return None

    config = model_registry.resolve(*model_registry.default_hint())  # medium tier
    if not config.model:
        logger.info("skills.generalize: medium tier not configured — synthesis skipped")
        return None
    try:
        provider = llm_client.get_provider(config)
    except RuntimeError as exc:
        logger.info("skills.generalize: no provider (%s) — synthesis skipped", exc)
        return None

    parts = []
    if current_body_text:
        parts.append("CURRENT PROCEDURE:\n" + current_body_text)
    parts.append(
        "SUCCESSFUL TRANSCRIPTS (most recent last):\n\n"
        + "\n\n---\n\n".join(_clip(t) for t in success_transcripts[-_MAX_TRANSCRIPTS:])
    )
    if failure_transcripts:
        parts.append(
            "FAILED ATTEMPTS (for the notes — do not put these steps in the body):\n\n"
            + "\n\n---\n\n".join(_clip(t) for t in failure_transcripts[:_MAX_TRANSCRIPTS])
        )

    try:
        result = await provider.summarize_text(
            system_prompt=_SYSTEM_PROMPT,
            user_content="\n\n".join(parts),
            model=config.model,
            max_tokens=_MAX_TOKENS,
        )
    except Exception:  # noqa: BLE001 - network/API failure
        logger.warning("skills.generalize: model call failed", exc_info=True)
        return None

    spec = _extract_json(result.content)
    if spec is None or not _valid(spec):
        logger.warning("skills.generalize: unparseable/invalid output: %r", (result.content or "")[:200])
        return None
    return _normalize(spec)
