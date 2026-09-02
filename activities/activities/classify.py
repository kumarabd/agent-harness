"""ClassifyRequest activity — step 2 of the request pipeline
(docs/components/request-pipeline/02-request-understanding.md).

A single cheap, fast-tier LLM call at the start of every top-level turn that
turns the inbound user message into a small task representation:

  - intent      — routing scalar
  - complexity  — routing scalar
  - confidence  — the classifier's own confidence (0.0–1.0)
  - retrieval_query — a distilled search query for steps 4/5/7
  - entities    — named systems/tools/files/people

All five are small routing signals *derived* from the message, not the
message itself — they ride back to the workflow as the activity's result
and `RetrievalWorkflow` passes `retrieval_query`/`entities` straight into
the retrieval activities. Nothing is persisted here; the bulk retrieved
*content* (memory items, composed skill, tool schemas) is what goes through
the `turn_retrieval` staging table, in later steps.

Design principles:

1. **Fast tier, not medium.** `model_registry.resolve("language", "fast")` —
   a low-stakes structured-extraction call, not on the reasoning path.

2. **No fallback. If it can't classify, it fails.** An unconfigured `fast`
   tier, a provider error, an unparseable response, or output that doesn't
   match the contract all raise `ClassificationError`. There is deliberately
   no neutral/degraded representation — a broken classifier is a broken
   turn, which surfaces (the turn fails, `turns.status='failed'`, Temporal
   records it) and gets fixed at the source. Silently routing every turn as
   `(task, moderate)` because the classifier is down is exactly the kind of
   invisible degradation this system does not do.

3. **Tolerant extraction, strict validation.** The model is asked for a bare
   JSON object; extraction strips markdown fences and pulls the first object
   out of surrounding prose (a well-intentioned response in slightly the
   wrong shape). But once extracted, every field is validated against its
   contract — an out-of-set enum, a non-numeric confidence, a missing
   `retrieval_query` — and anything that doesn't hold raises rather than
   being coerced to a default.
"""

from __future__ import annotations

import json
import logging
import re
import time

from temporalio import activity

from . import ids, llm_client, model_registry
from .types import ClassifyRequestInput, TaskRepresentation

logger = logging.getLogger(__name__)


class ClassificationError(RuntimeError):
    """Raised whenever ClassifyRequest cannot produce a real task
    representation — unconfigured tier, provider error, unparseable response,
    or output that violates the contract. There is no fallback; the activity
    raises, Temporal retries the bounded ladder, and an exhausted retry fails
    the turn (turn.go). Deliberate: a broken classifier must be visible, not
    papered over with a neutral guess."""


# --- Taxonomy: the single source of truth for both closed enums. ---
INTENTS = ("conversational", "question", "task", "meta")
COMPLEXITIES = ("trivial", "simple", "moderate", "complex")

# model-registry.md's tiers — fast is the right home for a cheap structured
# extraction call that isn't on the reasoning path. The `fast` tier MUST be a
# non-thinking model: the completion is a ~5-field JSON object, and a thinking
# model burns this budget on reasoning tokens, truncates the JSON, and now
# (no fallback) fails the turn. That failure is the intended signal — fix the
# tier, don't inflate the budget to hide a wrong model choice.
_CLASSIFIER_TIER = "fast"
_CLASSIFIER_MAX_TOKENS = 400

# Bounds — a task representation is routing metadata plus one short query,
# not a second copy of the conversation. Same numeric-tuning discipline as
# elsewhere in this project: revisit once real usage data exists.
_MAX_MESSAGE_CHARS = 8_000
_MAX_RECENT_MESSAGES = 4
_MAX_RECENT_MESSAGE_CHARS = 200
_MAX_RETRIEVAL_QUERY_CHARS = 256
_MAX_ENTITIES = 12
_MAX_ENTITY_CHARS = 64

_CLASSIFIER_SYSTEM_PROMPT = (
    "You analyse a user's latest message to a general-purpose personal-assistant "
    "agent, so the agent can route and retrieve for the request. Respond with ONLY "
    "a single JSON object — no prose, no markdown fences — with exactly these fields:\n"
    '- "intent": one of "conversational" (greetings, chit-chat, acknowledgements), '
    '"question" (wants information or an explanation, no action on external systems), '
    '"task" (wants the agent to do something: create, change, run, fix, find-and-act), '
    '"meta" (about the agent itself — its capabilities, memory, settings).\n'
    '- "complexity": one of "trivial" (one step, no ambiguity), "simple" (a few '
    'obvious steps), "moderate" (needs some planning or judgement), "complex" '
    "(multi-stage, significant reasoning or many steps).\n"
    '- "retrieval_query": a single concise search query (max ~20 words) capturing '
    "what to look up in memory and skills for this request. Rephrase the user's "
    "intent as a clean query and resolve references to the recent conversation "
    '(e.g. "yes do that" -> the actual task). Do not just copy the message.\n'
    '- "entities": array of short strings — named systems, tools, services, files, '
    'or people referenced (e.g. "Grafana", "Prometheus", "auth module"). Empty '
    "array if none.\n"
    '- "continues_prior": boolean. If an "In-progress task" block is shown below, '
    "true means this message continues that same task (an answer to the agent's "
    "question, more detail, a correction, a next step of the same work); false "
    "means it starts a different task. Always false when no in-progress task is "
    "shown.\n"
    '- "confidence": number between 0.0 and 1.0 — your confidence in this analysis.'
)


def build_classifier_user_content(
    user_message: str, recent_context: str, open_task: str = ""
) -> str:
    """The user-role content for the classifier call — the latest message
    plus a short recent transcript so `retrieval_query` can resolve
    follow-up references, plus (when one is open) a one-line summary of the
    task the agent is mid-way through, so `continues_prior` can be judged.
    Pure, separately testable."""
    message = _truncate(user_message.strip(), _MAX_MESSAGE_CHARS)
    recent = recent_context.strip() or "(none — this is the first message in the session)"
    parts = [f"Recent conversation (oldest to newest):\n{recent}"]
    if open_task.strip():
        parts.append(f"In-progress task the agent is working on:\n{open_task.strip()}")
    parts.append(f"Latest message:\n{message}")
    return "\n\n".join(parts)


def parse_task_representation(raw: str) -> TaskRepresentation:
    """Extract and validate the classifier's response. Extraction is tolerant
    (markdown fences, prose around the object); validation is strict — every
    field must match its contract or this raises `ClassificationError`. There
    is no per-field fallback. Pure, separately testable."""
    obj = _extract_json_object(raw)
    if obj is None:
        raise ClassificationError(f"classifier output was not a JSON object: {raw[:200]!r}")

    intent = _require_enum(obj.get("intent"), INTENTS, "intent")
    complexity = _require_enum(obj.get("complexity"), COMPLEXITIES, "complexity")
    confidence = _require_confidence(obj.get("confidence"))

    query = obj.get("retrieval_query")
    if not isinstance(query, str) or not query.strip():
        raise ClassificationError(f"retrieval_query missing or not a non-empty string: {query!r}")

    continues_prior = obj.get("continues_prior")
    if not isinstance(continues_prior, bool):
        raise ClassificationError(f"continues_prior missing or not a boolean: {continues_prior!r}")

    return TaskRepresentation(
        intent=intent,
        complexity=complexity,
        confidence=confidence,
        retrieval_query=_truncate(query.strip(), _MAX_RETRIEVAL_QUERY_CHARS),
        entities=_clean_entities(obj.get("entities")),
        continues_prior=continues_prior,
    )


# --- small pure helpers -----------------------------------------------------


def _truncate(text: str, limit: int) -> str:
    return text if len(text) <= limit else text[:limit] + "…"


def _extract_json_object(raw: str) -> dict | None:
    text = (raw or "").strip()
    if text.startswith("```"):
        text = re.sub(r"^```[a-zA-Z0-9]*\n?", "", text)
        text = re.sub(r"\n?```$", "", text).strip()
    try:
        obj = json.loads(text)
        return obj if isinstance(obj, dict) else None
    except (json.JSONDecodeError, ValueError):
        pass
    start, end = text.find("{"), text.rfind("}")
    if start != -1 and end > start:
        try:
            obj = json.loads(text[start : end + 1])
            return obj if isinstance(obj, dict) else None
        except (json.JSONDecodeError, ValueError):
            return None
    return None


def _require_enum(value: object, allowed: tuple[str, ...], field: str) -> str:
    if isinstance(value, str) and value.strip().lower() in allowed:
        return value.strip().lower()
    raise ClassificationError(f"{field}={value!r} is not one of {allowed}")


def _require_confidence(value: object) -> float:
    try:
        parsed = float(value)  # type: ignore[arg-type]
    except (TypeError, ValueError):
        raise ClassificationError(f"confidence={value!r} is not a number") from None
    # A number slightly outside [0, 1] is a range slip, not a broken response —
    # clamp it. A non-number is a broken response — raised above.
    return max(0.0, min(1.0, parsed))


def _clean_entities(value: object) -> list[str]:
    """Entities are advisory extras, not a routing scalar — an absent or
    malformed list is tolerated (empty), the well-formed part is bounded.
    This is the one field with no hard contract to violate."""
    if not isinstance(value, list):
        return []
    out: list[str] = []
    for item in value:
        if isinstance(item, str) and item.strip():
            out.append(_truncate(item.strip(), _MAX_ENTITY_CHARS))
        if len(out) >= _MAX_ENTITIES:
            break
    return out


class ClassifyRequestActivity:
    """Bound-method activity — Postgres pool injected per-process (for the
    seed-message and recent-context reads), same pattern as
    `ModelCallActivity` / `ToolCallActivity`. The provider client is resolved
    per call via `llm_client.get_provider` so the `fast` tier's own config is
    used (and cached)."""

    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="ClassifyRequest")
    async def __call__(self, input: ClassifyRequestInput) -> TaskRepresentation:
        meter = activity.metric_meter()
        started = time.monotonic()

        # No try/except here on purpose: a ClassificationError propagates out
        # of the activity, Temporal retries the bounded ladder (turn.go's
        # ClassifyRequest ActivityOptions), and an exhausted retry fails the
        # turn. Only *successful* classifications reach the metric — a
        # persistent classifier failure shows up as a spike in Temporal's
        # activity-failure metrics and failed turns, which is the point.
        representation = await self._classify(input.turn_id)

        tagged = meter.with_additional_attributes({"intent": representation.intent})
        tagged.create_histogram_float("classify_request_latency_seconds", unit="s").record(
            time.monotonic() - started
        )
        tagged.create_counter("classify_request_total").add(1)
        return representation

    async def _classify(self, turn_id: str) -> TaskRepresentation:
        async with self._pool.acquire() as conn:
            seed = await conn.fetchrow(
                "SELECT t.turn_seq, m.content "
                "FROM turns t JOIN messages m ON m.parent_id = t.turn_id "
                "WHERE t.turn_id = $1 AND m.seq = 0 AND m.role = 'user'",
                turn_id,
            )
            if seed is None or not seed["content"]:
                # InsertMessage runs before ClassifyRequest and creates this
                # row — its absence is a broken invariant, not a case to
                # smooth over.
                raise ClassificationError(f"no seed user message for turn {turn_id}")
            user_message: str = seed["content"]
            recent_context = await self._recent_context(conn, ids.session_key_of(turn_id), seed["turn_seq"])
            open_task = await self._open_task_summary(conn, ids.session_key_of(turn_id), turn_id)

        config = model_registry.resolve("language", _CLASSIFIER_TIER)
        if not config.model:
            raise ClassificationError(
                f"{_CLASSIFIER_TIER} language tier is not configured — cannot classify"
            )
        try:
            provider = llm_client.get_provider(config)
        except RuntimeError as exc:
            raise ClassificationError(f"no provider for {_CLASSIFIER_TIER} tier: {exc}") from exc

        # A network/API failure propagates unchanged — Temporal's retry ladder
        # handles the transient case, an exhausted retry fails the turn.
        result = await provider.summarize_text(
            system_prompt=_CLASSIFIER_SYSTEM_PROMPT,
            user_content=build_classifier_user_content(user_message, recent_context, open_task),
            model=config.model,
            max_tokens=_CLASSIFIER_MAX_TOKENS,
        )

        representation = parse_task_representation(result.content)
        logger.info(
            "classify[%s]: intent=%s complexity=%s conf=%.2f query=%r entities=%s",
            turn_id,
            representation.intent,
            representation.complexity,
            representation.confidence,
            representation.retrieval_query,
            representation.entities,
        )
        return representation

    async def _open_task_summary(self, conn, session_key: str, turn_id: str) -> str:
        """One line describing the top-level episode the agent is currently
        mid-way through (docs/components/episode-lifecycle.md), for the
        `continues_prior` judgement. Empty when none is open, or when the open
        one IS this turn's own episode (a mid-turn re-classify — shouldn't
        happen, but harmless)."""
        row = await conn.fetchrow(
            "SELECT e.episode_id, e.retrieval_query FROM episodes e "
            "JOIN turns t ON t.turn_id = e.episode_id "
            "WHERE e.session_key = $1 AND e.status = 'open' AND t.parent_type = 'session' "
            "ORDER BY e.opened_at DESC LIMIT 1",
            session_key,
        )
        if row is None or row["episode_id"] == turn_id:
            return ""
        last_assistant = await conn.fetchrow(
            "SELECT m.content FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
            "WHERE t.episode_id = $1 AND m.role = 'assistant' AND m.content IS NOT NULL "
            "ORDER BY t.turn_seq DESC, m.seq DESC LIMIT 1",
            row["episode_id"],
        )
        line = (row["retrieval_query"] or "").strip() or "(task in progress)"
        if last_assistant and last_assistant["content"]:
            line += f"\nagent's last message: {_truncate(last_assistant['content'].strip(), _MAX_RECENT_MESSAGE_CHARS)}"
        return line

    async def _recent_context(self, conn, session_key: str, turn_seq: int | None) -> str:
        """A short tail of the prior conversation in this session — user and
        assistant messages from earlier top-level turns, chronological.
        Empty for the session's first turn."""
        if turn_seq is None or turn_seq <= 1:
            return ""
        rows = await conn.fetch(
            "SELECT m.role, m.content "
            "FROM messages m JOIN turns t ON m.parent_id = t.turn_id "
            "WHERE t.parent_id = $1 AND t.parent_type = 'session' AND t.turn_seq < $2 "
            "  AND m.role IN ('user', 'assistant') AND m.content IS NOT NULL "
            "ORDER BY t.turn_seq DESC, m.seq DESC "
            "LIMIT $3",
            session_key,
            turn_seq,
            _MAX_RECENT_MESSAGES,
        )
        if not rows:
            return ""
        return "\n".join(
            f"{row['role']}: {_truncate((row['content'] or '').strip(), _MAX_RECENT_MESSAGE_CHARS)}"
            for row in reversed(rows)
        )
