"""LCM (Lossless Context Management) — docs/components/context-slot.md,
"Resolved: LCM as the Concrete Mechanism" (Ehrlich & Blackman 2026). Owns
session-scoped context assembly, compression, and retrieval; nothing else
touches context_summaries or does session-wide message assembly directly —
compress_context.py, llm.py, and seed_child_session.py all call into this
package rather than duplicating logic (clean separation, one place to read
to understand the whole mechanism).

Package split (2026-08-29), replacing the former flat lcm.py, one file per
concern:
  - constants.py — shared tunables + estimate_tokens, imported by all three.
  - assembly.py  — assemble() (context construction for a live model call).
  - compaction.py — compression_state()/compact() (the write side: folding
    old messages into summaries).
  - retrieval.py — grep()/describe()/expand() (the Memory-Access Tools —
    docs/components/context-slot.md's paper-comparison writeup — the read
    side: pulling specific things back out of the DAG on demand).

This __init__ re-exports the exact flat surface every existing caller
already uses (`from . import lcm` then `lcm.assemble(...)`,
`lcm.compact(...)`, `lcm.estimate_tokens(...)`) unchanged, so the package
split is invisible to compress_context.py/llm.py/seed_child_session.py —
none of them needed to change. tools.py imports the new
grep/describe/expand names directly for the three Memory-Access Tool
handlers.
"""

from __future__ import annotations

from .assembly import assemble, session_messages
from .compaction import compact, compression_state
from .constants import (
    GREP_DEFAULT_LIMIT,
    LEAF_FOLD_THRESHOLD,
    VERBATIM_WINDOW_MESSAGES,
    estimate_tokens,
)
from .retrieval import LCMNotFoundError, describe, expand, grep

__all__ = [
    "assemble",
    "session_messages",
    "compact",
    "compression_state",
    "GREP_DEFAULT_LIMIT",
    "LEAF_FOLD_THRESHOLD",
    "VERBATIM_WINDOW_MESSAGES",
    "estimate_tokens",
    "LCMNotFoundError",
    "describe",
    "expand",
    "grep",
]
