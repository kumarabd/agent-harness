"""Claim-check store for large tool outputs — closes
docs/components/session-filesystem.md's "Resolved: This PV Serves as the
Claim-Check Store for Large Content" gap (previously "not yet implemented"),
and delivers on docs/components/temporal-workflow.md's reference-passing
contract for the specific case of tool outputs too big to inline into a
tool_calls.result row.

Mechanism: when a tool's output exceeds SMALL_OUTPUT_BYTES, the full bytes
are written to the tool's own session working directory (under a
`.claim-check/` subdirectory keyed by tool_call_id), and the result the
model actually sees is a reference — a small preview (head + tail) plus
the real path — instead of the raw blob. The model then uses ordinary
`shell_exec` (`cat`, `head`, `tail`, `grep`) to read whichever part of
the output it needs; nothing new for the model to learn.

Location choice: the `.claim-check/` directory sits inside the calling
turn/subagent's OWN session directory, not at a shared session root.
This matches this repo's session filesystem isolation model directly
(components/session-filesystem.md, "Responsibilities"): a subagent
already can't see the parent's session tree, and shouldn't be able to see
the parent's tool outputs either — placing the artifact next to the tool
call that produced it keeps that boundary intact without a second
mechanism. A leading dot keeps it out of casual `ls` output and out of
the subagent-manifest walk (subagent_manifest.py already skips it) so
tool-output artifacts don't get mistaken for user-facing changed files
during a merge.

Threshold: SMALL_OUTPUT_BYTES matches the pre-existing inline safety cap
in tools.py (previously the only bound — output past this was silently
truncated and dropped). "Above this" now means "route through the PV,"
not "lose it." The exact number is deliberately placeholder-simple, same
numeric-tuning discipline this project applies everywhere else pending
real usage data.

Preview shape: HEAD_BYTES from the start + TAIL_BYTES from the end
gives the model both the tool's opening lines (usually a version banner,
usage message, or the shape of the output) AND its trailing lines
(usually the diagnostic — an error message, a summary, or the last few
records processed). The previous truncation was head-only, silently
losing exactly the diagnostic tail — a real information loss beyond
"we couldn't fit it," which the split preview closes even for content
the model chooses never to open via the reference path.

Not built here (deliberately): the type-aware Exploration Summary
context-slot.md, "Resolved: Duties and Strategies" #2 calls for
(schema/shape extraction for structured data, structural analysis for
code, LLM summary for unstructured text — LCM §2.2). That's a
context-slot concern about how to REPRESENT large content in the
prompt over time; this module is only the storage/reference plumbing
underneath it. When Exploration Summary lands, it produces its summary
FROM a claim-check reference this module already wrote, without needing
to change the storage layer.
"""

from __future__ import annotations

import logging
import os

from . import exploration_summary

logger = logging.getLogger(__name__)

# The pre-existing inline safety cap from tools.py, reused verbatim as
# the "small enough to inline" threshold. Not sized against real usage
# data — treat as placeholder, revisit alongside the doc's own
# still-open "large-vs-small threshold" question.
SMALL_OUTPUT_BYTES = 4096

# Split evenly under SMALL_OUTPUT_BYTES so the total preview payload
# handed back to the model stays under the same inline budget the
# previous truncation used. Head+tail because a runaway command's
# diagnostic is usually at the end (error message, summary, the last
# record processed), which head-only truncation silently dropped.
HEAD_BYTES = SMALL_OUTPUT_BYTES // 2
TAIL_BYTES = SMALL_OUTPUT_BYTES // 2

# `.claim-check/` sits inside the calling turn/subagent's own session
# directory (session_dir on ToolContext), NOT at a session-wide root —
# matches session-filesystem.md's isolation model exactly. Leading dot
# keeps it out of casual `ls` and out of the subagent-manifest walk.
_CLAIM_CHECK_DIRNAME = ".claim-check"


def _sanitize_id(tool_call_id: str) -> str:
    """`{turn_id}:act:{n}` contains colons — legal on POSIX but ugly and
    a footgun for anything shell-quoting the filename later. Underscores
    are unambiguous."""
    return tool_call_id.replace(":", "_")


def _preview(text: str) -> tuple[str, str]:
    """Head+tail split. Falls back to empty tail if the content is
    shorter than head+tail combined — no overlap, no double-count."""
    encoded = text.encode("utf-8")
    if len(encoded) <= HEAD_BYTES + TAIL_BYTES:
        return text, ""
    head = encoded[:HEAD_BYTES].decode("utf-8", errors="replace")
    tail = encoded[-TAIL_BYTES:].decode("utf-8", errors="replace")
    return head, tail


async def store_if_large(
    session_dir: str,
    tool_call_id: str,
    stream_name: str,
    data: bytes,
    summary_provider=None,
    summary_model: str = "",
) -> dict:
    """If data fits inline, returns {"inline": text}. Otherwise writes it
    to `.claim-check/{tool_call_id}.{stream_name}.log` under session_dir
    and returns a reference: path (relative to session_dir, since that's
    what the model can pass to `cat`/`head`/`tail` from a subsequent
    `shell_exec`), size, a head+tail preview, and a type-aware
    exploration_summary (docs/components/context-slot.md, "Resolved:
    Duties and Strategies" duty #2 — schema/shape for structured data,
    LLM-backed summary for unstructured text).

    Called under the tool's already-held session-directory lease — no
    additional coordination needed, since this file is owned by exactly
    one tool call (keyed by its own tool_call_id) and only ever written
    once. The lease coordinates the shared session directory itself; a
    per-file lease for the claim-check artifact would be redundant.

    Async because the exploration_summary text branch may make an LLM
    call. summary_provider/summary_model both optional — a caller in a
    fixture-only environment (no real provider configured) passes
    neither, and the summary degrades to deterministic-only fields
    without an LLM round-trip. Same graceful-degradation pattern
    agent_brain.py already uses for its own unconfigured case.
    """
    if len(data) <= SMALL_OUTPUT_BYTES:
        return {"inline": data.decode("utf-8", errors="replace")}

    claim_check_dir = os.path.join(session_dir, _CLAIM_CHECK_DIRNAME)
    os.makedirs(claim_check_dir, exist_ok=True)

    filename = f"{_sanitize_id(tool_call_id)}.{stream_name}.log"
    absolute_path = os.path.join(claim_check_dir, filename)
    with open(absolute_path, "wb") as f:
        f.write(data)

    text = data.decode("utf-8", errors="replace")
    head, tail = _preview(text)
    relative_path = os.path.join(_CLAIM_CHECK_DIRNAME, filename)

    # Exploration Summary runs against the same in-memory bytes we just
    # wrote — no second disk read. Never raises (see
    # exploration_summary.summarize's contract), so failure here is a
    # bug in that module, not something claim_check needs to defend
    # against.
    summary = await exploration_summary.summarize(data, provider=summary_provider, model=summary_model)

    logger.info(
        "claim_check.store_if_large: wrote %d bytes to %s (%s, summary=%s)",
        len(data),
        absolute_path,
        stream_name,
        summary.get("type"),
    )
    return {
        "claim_check_path": relative_path,
        "size_bytes": len(data),
        "head": head,
        "tail": tail,
        "exploration_summary": summary,
        "note": (
            f"Output exceeded {SMALL_OUTPUT_BYTES} bytes and was stored at "
            f"{relative_path} in the session working directory. Use shell_exec "
            f"(cat, head, tail, grep) against that path to inspect the full "
            f"content; exploration_summary gives a type-aware shape/schema "
            f"description for structured data or a natural-language summary "
            f"for unstructured text."
        ),
    }


def is_claim_check_dir(name: str) -> bool:
    """Predicate for subagent_manifest.py / merge_subagent_output — used to
    prune this directory out of `os.walk` so claim-check artifacts don't
    surface as subagent-authored changed files during a merge."""
    return name == _CLAIM_CHECK_DIRNAME
