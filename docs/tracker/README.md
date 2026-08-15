# Tracker

> Living functionality-diff trackers against reference harnesses. Unlike `03-architecture-comparison-vs-hermes.md` (which records the *rationale* for specific design decisions we already made), these docs track ongoing **feature parity / gap status** against each reference project — updated as either side changes, not a one-time comparison.

### Purpose
As our harness design evolves, keep an explicit running tally of:
- What a reference harness (Hermes, OpenClaw, ...) does that we've matched, haven't matched, or have deliberately decided not to match.
- What we do that they don't.
- Where our understanding of the reference project itself might be stale (these projects move too).

### How to use this
- One file per reference harness.
- Each file is a checklist/table, not prose — status per feature area, with a one-line note and a date.
- When a gap is closed (we build it) or a reference project changes, update the row and bump the "Last verified" date at the top of that file rather than leaving stale claims in place.
- If a claim about the reference project hasn't been re-checked against its actual current source/docs in a while, mark it `UNVERIFIED` rather than asserting it as current fact — matches the caution already applied to the Hermes claims throughout the rest of `docs/`.

### Files
- [`hermes.md`](./hermes.md) — Hermes Agent (NousResearch). Baseline this whole redesign extends; see `../01-architecture-overall-topology.md` and `../03-architecture-comparison-vs-hermes.md` for the deep-dive rationale behind each divergence.
- [`openclaw.md`](./openclaw.md) — OpenClaw. Referenced once so far (`../01-architecture-overall-topology.md` line 20) as a peer reason-act-observe coding harness; no detailed comparison exists yet — scaffolded, needs a real research pass.
