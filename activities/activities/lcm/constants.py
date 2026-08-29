"""Shared constants and the token-estimation heuristic — split out of the
former flat lcm.py so every submodule in this package (assembly, compaction,
retrieval) imports from one place rather than each other."""

from __future__ import annotations

# All placeholder-simple, matching this project's standing practice of
# deferring precise numeric tuning until real usage data exists (same shape
# as turn.go's own budgetTokens/softCompressionThreshold/hardCompressionThreshold).
VERBATIM_WINDOW_MESSAGES = 20  # last K reasoning steps stay verbatim, uncompressed
LEAF_FOLD_THRESHOLD = 5  # fold this many uncombined leaf summaries into one condensed summary

# docs/components/context-slot.md's own "Resolved: Recall Latency"-adjacent
# discipline: a real starting value for lcm_grep's default result count, not
# deferred — see retrieval.py's own comment for the reasoning (a default
# that prevents accidental context flooding on an unscoped query, with no
# hard system-enforced ceiling above it).
GREP_DEFAULT_LIMIT = 20


def estimate_tokens(text: str) -> int:
    """Plain character-based heuristic (len(text)//4), not a real tokenizer
    — the actual model varies per tenant (Pioneer, Crusoe/DeepSeek, ...) and
    there's no single correct tokenizer to target without knowing which; a
    threshold check needs a reasonable proxy, not an exact count, matching
    this project's existing tolerance for approximate constants in this
    exact area (turn.go's own budgetTokens is a "high placeholder ceiling",
    not derived from anything precise either)."""
    return max(1, len(text or "") // 4)
