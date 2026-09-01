# Request Pipeline — Step 4: Memory Retrieval

> STATUS: IMPLEMENTED — `activities/activities/retrieval/memory.py`. Calls
> `agent_brain.memory_search`, dedups + budgets the fused results, stages
> `kind='memory'` rows; `llm.build_conversation` reads them every ModelCall.
> The old `turn_seq==1` in-process retrieval in `llm.py`
> (`_session_start_memory_block` / `_render_memory_results`) is deleted. MMR
> diversity and a real relevance floor are still open (see below).
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Orchestrated by [`03-routing.md`](03-routing.md)'s `RoutingWorkflow`.
> Backend contract: [`../memory-slot.md`](../memory-slot.md).

### Role

Pull the **user/tenant adaptation layer** relevant to this task from agent-brain
— corrections, settled decisions, persona/behaviour rules, environment
domain-facts, learned tool strategy, episodic precedent, conversational
continuity. Distinct from within-session context (LCM / `context-slot.md`) and
from the skill (task-shaped). This is what specializes a generic procedure — or
a bare no-skill turn — to *this* user.

### Activation

Runs for anything except a high-confidence `conversational` turn. Default-on is
consistent with `memory-slot.md`'s "session-start retrieval is unconditional,
accept the wasted call" — but now **task-scoped**, so it also fires on a
mid-session topic shift, not just the session's first turn.

**Subsumed** the hardcoded `turn_seq == 1` retrieval trigger that used to live
in `llm.build_conversation` — session-start is now just the case where the task
query is the first message. `build_conversation`'s bespoke
`_session_start_memory_block` / `_render_memory_results` / retry loop are
deleted; it now reads the staged `turn_retrieval` `kind='memory'` rows on
**every** ModelCall (they don't change within a turn), so the background block
is present for the whole turn — the old path only injected it on the very first
ModelCall and it vanished thereafter.

### `MemoryRetrieve` activity

- **Runs on:** tenant-worker (agent-brain credentials).
- **Input:** `{turn_id, retrieval_query}` — the distilled query from step 2's
  `TaskRepresentation`, passed straight in by `RoutingWorkflow` (a small
  derived signal, no Postgres round-trip).
- **Mechanism:** `agent_brain.call_tool("memory_search", {query, limit: 15})` —
  already RRF-fused server-side across agent-brain's six retrieval surfaces
  (`emu`, `semantic_nodes`, `domain_edges`, `facts`, `semantic_rules`,
  `concept_definitions`).
- **Per-result text** (`_item_text`) — picks the one useful line per fused
  source: `statement`, or `term: definition`, or `subject predicate object` from
  `emu.semantic_fact`, or a bare `content`. A result with none is dropped.
- **Selection** (`_select`) — over agent-brain's already-ranked list, preserving
  order: normalized-text exact dedup, a relevance floor (`_RELEVANCE_FLOOR`,
  currently `0.0` — a no-op until agent-brain's score scale is pinned down),
  and a token budget (`_TOKEN_BUDGET = 1500`, `lcm.estimate_tokens`).
- **Output:** one `turn_retrieval` row per surviving item — `kind='memory'`,
  `seq` = rank, `content` = the line, `score` = the fused score if present,
  `metadata = {source, id}`.
- **Status:**
  - `ok` — staged ≥ 1 row.
  - `empty` — `AGENT_BRAIN_*` unset, empty query, or no usable results.
  - `error` — configured, call failed. **Raised** (no in-activity retry) so
    `RoutingWorkflow`'s `RetryPolicy{MaximumAttempts: 3}` retries, then records
    `error`.
  - `timed_out` — didn't settle before `RoutingWorkflow`'s phase deadline.
- **Not done:** `memory_expand` on top candidates; MMR-style diversity (needs
  embeddings); contradiction reconciliation (deferred to agent-brain's
  bi-temporal validity — a superseded rule shouldn't be returned in the first
  place, see [`06-skill-composition.md`](06-skill-composition.md)); the
  `harness_type` typing (not implementable against agent-brain's real schema —
  see `memory-slot.md`).

### What the planner / assembly does with it

The composed skill (step 6) consumes memory items to fill slots and adapt steps.
Items not consumed by a skill are still placed in the assembled prompt as a
labelled background block, before the live conversation — `memory-slot.md`'s
"Resolved: Staleness Is Handled by Placement".

### Relationship to `memory-slot.md`

This phase *is* `memory-slot.md`'s runtime retrieval, promoted to a task-scoped
pipeline step with its own activation gate. Nothing about agent-brain's contract
(`memory_search` / `memory_expand`, two-tier shallow/deep, no interface
abstraction) changes. The mid-session model-initiated `memory_search` **tool**
also stays — this phase is the automatic complement, not a replacement.

### Open Questions

- Whether `memory_expand` is worth the extra round-trip in the automatic path,
  or left to the model-initiated tool.
- agent-brain's fused-result **score field name and scale** — `_score` probes
  `score` / `rrf_score` / `fused_score` / `rank_score` defensively; once the
  real shape is confirmed, set a meaningful `_RELEVANCE_FLOOR`.
- MMR diversity — deferred until the fused list is shown to be redundant in
  practice (needs an embedding call per candidate).
- Budget / floor thresholds — numeric-tuning discipline.
