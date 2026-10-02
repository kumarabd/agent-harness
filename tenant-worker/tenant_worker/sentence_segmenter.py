"""Real sentence-boundary detection for ModelCall streaming
(docs/components/gateway.md's "Resolved: ModelCall Streaming" — "a text
segmenter flushing to TTS on real sentence boundaries, not naive
period-splitting — 'Dr. Smith said...' isn't one").

Not a full NLP sentence tokenizer — a lightweight, real heuristic: a
sentence-ending punctuation mark followed by whitespace is a candidate
boundary, suppressed when the word immediately before it is a known
abbreviation or looks like an initial (a single letter, which also happens
to catch the last segment of multi-period abbreviations like "U.S."). Good
enough for gating when to flush a chunk to delivery; not a claim of
linguistic correctness.
"""

from __future__ import annotations

import re

_ABBREVIATIONS = {
    "mr", "mrs", "ms", "dr", "prof", "sr", "jr", "st", "vs", "etc",
    "fig", "no", "vol", "approx", "dept", "univ", "inc", "ltd", "co",
    "eg", "ie",
}

# A run of sentence-ending punctuation, an optional closing quote/bracket,
# then real whitespace — the whitespace requirement is what naturally
# excludes "3.14" or "U.S." (no space between the periods) from ever being
# candidate boundaries in the first place, no separate digit/abbreviation
# check needed for those cases.
_SENTENCE_END = re.compile(r'[.!?]+["\')\]]?(\s+)')


def find_boundary(buffer: str) -> int | None:
    """Return the index just past the first confident sentence boundary in
    buffer (including its trailing whitespace), or None if none exists yet.
    Caller flushes buffer[:index] and keeps buffer[index:] for next time.
    """
    for m in _SENTENCE_END.finditer(buffer):
        punct_start = m.start()
        word_match = re.search(r"(\w+)$", buffer[:punct_start])
        if word_match:
            word = word_match.group(1).lower()
            if word in _ABBREVIATIONS:
                continue
            if len(word) == 1 and word.isalpha():
                continue  # a single-letter initial, e.g. "J." in "J. K. Rowling"
        return m.end()
    return None
