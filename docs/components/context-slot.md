# Component: Context Slot
## (Short-Term Memory — Session-Scoped Working Context)

> STATUS: IN PROGRESS — scope, compression strategy, retrieval triggers, system-prompt structure, summary storage schema, and session-start retrieval gating resolved (via LCM, Ehrlich & Blackman 2026). Topic drift and threshold sizing remain open, deliberately deferred pending real usage data.

### Role (one line)
Owns what actually goes into a turn's `ModelCall` prompt — assembly, bounding, and compression of the live working context, scoped to the **whole session**, not one turn — replacing today's unbounded single-turn verbatim replay and no-op compression stub with a mechanism that can survive a genuinely long-lived, multi-topic session.

### Why this exists (recap)
Two concrete gaps found while auditing the implementation:
- `activities/activities/llm.py`'s `build_conversation` replays every message and tool result for the *current turn only* (`WHERE parent_id = turn_id`), verbatim, unbounded.
- `activities/activities/compress_context.py`'s `CompressContext` activity is a literal no-op, already wired into the turn loop's token-threshold gate (`components/temporal-workflow.md`, "Resolved: Compression / Context Management") but doing nothing when it fires.

### Resolved: Scope Is the Whole Session, Not One Turn
Originally scoped to a single turn (matching `build_conversation`'s current query). Revised after establishing two things:
- **The session idle timeout does not bound this.** `coordinator.go`'s `idleTTL` governs whether the *Coordinator Workflow execution* stays alive — when it fires, the workflow exits and a fresh `SignalWithStartWorkflow` recreates it later. Sessions themselves are **never deleted** (`components/session-filesystem.md`, `components/dreaming.md`), so a session used intermittently for months accumulates turns in Postgres indefinitely, completely independent of how many times its coordinator has idled out and respawned. Workflow-lifecycle and data-volume are unrelated axes.
- **Cross-turn continuity doesn't exist at all today, so this isn't scope creep — it's a real gap.** A new top-level `turn_id` is minted only when there's no currently-active turn (`coordinator.go`), and `build_conversation` is scoped strictly to one `turn_id`. So once a turn completes and the user sends a new message, that next turn starts with zero awareness of the just-finished exchange, even seconds later. Session-scoping this component is the fix.

**Where this draws the line against `components/memory-slot.md`:** context-slot = everything *within* this session, however long it actually lives; memory-slot = everything *across* sessions. This matches human working memory vs. long-term memory more cleanly than a turn/session split would — working memory spans a whole conversation regardless of how long it runs; long-term memory is what survives *between* conversations.

**Two concrete consequences for implementation (not yet made, doc-only pass):**
1. `build_conversation`'s query needs to change from `WHERE parent_id = turn_id` to pulling every top-level turn under the session (`JOIN turns WHERE parent_type = 'session' AND parent_id = session_key`, ordered by `turn_seq` then `seq`).
2. Token/budget accounting (`cumulativeTokens` in `turn.go`) currently lives in `TurnWorkflow`-local memory, which resets every new turn since each turn is a separate child-workflow execution. Session-level accounting can't be carried in workflow memory across that boundary — it has to be recomputed from Postgres at the start of each reasoning step, the same place `build_conversation` itself now reads from. This is consistent with how the rest of this project already treats Postgres as the source of truth, not a new pattern.

**Explicit non-goal:** session-scope means *across top-level turns*, not *into subagent internals*. Subagent context isolation is already a deliberate, resolved decision (`components/session-filesystem.md`) — a subagent's intermediate exploration stays private, folded back only via its explicit result. This component doesn't touch that.

### Resolved: Duties and Strategies

**1. Prompt assembly.** Hybrid sliding window + **hierarchical summary DAG** (not a single flat rolling-summary block, per LCM §2.1): the last *K* reasoning steps stay verbatim; older content is folded into leaf summaries (a span of messages), which themselves get folded into condensed summaries (a summary of summaries) as they age further — multi-resolution, so old content degrades gracefully instead of a single block growing without bound.

**2. Tool-output / large-content bounding.** Two tiers, not one blanket rule:
- Small tool outputs: deterministic head/tail truncation, no LLM call — first N + last M lines, explicit "X lines omitted" marker.
- Large content (big files, big datasets): never loaded into context directly. Represented by an opaque reference (ID + path) plus a **type-aware Exploration Summary** — schema/shape extraction for structured data, structural analysis for code, LLM summary for unstructured text (LCM §2.2). Reference, don't duplicate — reuses the session filesystem's existing claim-check/lease mechanism (`components/session-filesystem.md`, "Resolved: The Tenant PV Also Hosts... the Claim-Check Store for Large Content") rather than inventing new storage.

**3. Compression trigger + execution.** Two-tier threshold, not one constant (LCM §2.1, §2.3):
- **Soft threshold**: triggers async compaction — fires, doesn't block the turn.
- **Hard threshold**: blocks until compaction completes.
- **Execution — real body for `CompressContext`**, replacing the no-op stub, via **Three-Level Escalation** for guaranteed convergence: Level 1 (LLM summarize, preserve details) → if it fails to shrink the content, Level 2 (LLM summarize, aggressive bullet points) → if that still fails, Level 3 (deterministic truncate, no LLM, always shrinks). This solves a real, previously-unnamed failure mode: a summarization call producing output *longer* than its input ("compaction failure").

**4. Compression-threshold sizing.** Keep the static constant for now (today's `budgetTokens`/`compressionGateTokens` shape in `turn.go`); make it a function of the active model's real context window once `components/model-registry.md` exists. Not blocked on that component landing first — swap the source of truth when it does.

**5. Cache-prefix stability.** Compaction only ever rewrites the aging tail — the summary/DAG block and whatever's leaving the verbatim window — never reshuffles already-cache-stable earlier messages. The swap happens atomically between turns, so a compaction event costs at most one KV-cache regeneration, not per-message churn (LCM §2.4's footnote reasoning).

**6. Losslessness.** Nothing new needs to be built for this — it's already structurally true. Postgres's `messages`/`tool_calls` tables are already never deleted; they already *are* LCM's "Immutable Store." The Active Context (the assembled prompt) is a derived **view** over that data, not a second copy — compression only changes what's shown to the model, never what's stored.

**7. Scope boundary.** Session-scoped (§ above), explicitly excluding subagent-internal content.

### Resolved: Retrieval Is Event-Triggered, Not Continuous
Two triggers cause memory-slot content to enter context-slot's assembled prompt — no third, automatic, always-on relevance pass:
- **Session start** — automatic, no model decision. Gated on "zero prior turns exist for this `session_key`," not on Coordinator respawns (which are workflow-lifecycle noise, unrelated to whether this is genuinely a new line of inquiry).
- **Mid-session, model-initiated** — the model calls a tool (`components/tool-registry.md`'s concern — `search`/`expand`), reading `components/memory-slot.md`'s direct agent-brain integration, folded back as an ordinary tool observation. No special-casing versus any other tool call.

**Explicitly rejected: re-evaluating relevance on every `ModelCall`.** This would mean the set of included blocks could change every reasoning step, which directly breaks cache-prefix stability (duty #5) — every step becomes a fresh cache miss. Continuous re-judgment and cache stability are in direct tension; cache stability wins.

**Explicitly deferred, not designed this pass:** a cheap, deterministic (non-LLM), always-on relevance *pre-filter* — analogous to preattentive/"cocktail party effect" salience detection in human attention: continuous, but cheap and parallel, categorically different from expensive deliberate reasoning. It would decide *whether* to trigger retrieval, never touch the prompt prefix itself, and so wouldn't reintroduce the cache-busting problem the rejection above avoids. Parked as future work — not required for this design to function, and consistent with this project's standing discipline of not building ahead of real usage data.

**Removal is a separate concern from addition, and stays that way.** Relevance triggers what gets *added*. What gets *smaller over time* is handled entirely by duty #3's token-budget-driven compression, uniformly, regardless of whether content originated from the session's own turns or from a memory-slot retrieval. There is no second "is this still relevant" pass that actively evicts content — everything ages via the same mechanism.

### Resolved: System Prompt Is Tiered, Not Monolithic
Two tiers, not one flat string:
- **Core identity** — small, static, always present, every `ModelCall`, unconditionally. This is today's `DEFAULT_SYSTEM_PROMPT`/`sessions.system_prompt` mechanism (`activities/activities/llm.py`), unchanged in shape — just deliberately kept small. No retrieval, no selection logic; the cost of getting this wrong (dropping a core constraint mid-conversation) is too high to make it conditional, and it's cheap enough that there's no compression benefit to stripping it.
- **Situational content** — persona/behavior preferences and domain-specific framing are *not* crammed into the static system prompt. They're typed, selectively-retrieved content sourced from `components/memory-slot.md` (`type: persona-rule`, `type: domain-fact`, alongside `type: episode`), entering context via the same two triggers above, not a separate mechanism. Placed and labeled distinctly from live session turns (not silently merged) so the model's own recency/authority weighting can naturally resolve any tension between old retrieved content and what the session has since established.

This reframes "domain shift" (e.g. a health-related question pulling in health-appropriate framing) as an ordinary memory-slot retrieval, not a dynamic system-prompt rewrite — and reframes "not every behavior rule applies to every task" as the same relevance-filtering problem context-slot already solves for conversation history, just pointed at a second input.

### Resolved: LCM as the Concrete Mechanism
Adopted directly, not just as inspiration, with two adaptations for how this project actually runs:

| LCM concept | Maps to | Note |
|---|---|---|
| Immutable Store | Postgres `messages` / `tool_calls` | Already never deleted — nothing new to build |
| Active Context | Context-Slot's assembled prompt | Session-scoped here, not turn-scoped |
| Summary DAG (leaf / condensed) | Context-Slot's compression output | New `context_summaries` table — see "Resolved: Summary Storage Schema" below |
| Soft / hard threshold | Extends `compressionGateTokens` into two tiers | One constant today → soft (async) + hard (blocking) |
| Three-Level Escalation | Real body for `CompressContext` | Replaces today's no-op stub; guarantees convergence |
| `lcm_grep` / `lcm_expand` | New `components/tool-registry.md` tools over Postgres + the DAG | Both unrestricted — **revised**, matches `components/memory-slot.md`'s `search`/`expand` (also both unrestricted; agent-brain's `memory_expand` has no depth to escalate through, so LCM's subagent-only guardrail on `lcm_expand` doesn't apply the same way here) |
| The "engine" (one continuous process) | Recomputed per `ModelCall` activity, fresh from Postgres | Each turn is a separate Temporal child-workflow execution — no long-lived process to hold state in |

LCM's own paper states its storage layer only needs transactional writes, referential integrity, and indexed full-text search — *"any storage backend satisfying these properties would suffice."* The per-tenant Postgres this project already runs satisfies that natively; adopting LCM's architecture isn't a new infrastructure dependency, just new logic over data already being persisted.

### Resolved: Summary Storage Schema
A dedicated table, not reuse of `messages`. A summary is a genuinely different kind of object from a conversational message, and this project has consistently given different kinds of objects their own tables rather than overloading an existing one with sparse, conditional columns (`session_filesystem_leases`, `_test_scripted_responses`).

```sql
CREATE TABLE context_summaries (
  summary_id    uuid PRIMARY KEY,
  session_key   text NOT NULL,
  kind          text CHECK (kind IN ('leaf','condensed')),
  covers        uuid[],   -- leaf: message_ids it summarizes; condensed: child summary_ids
  content       text,     -- the actual summary text
  token_count   int,
  created_at    timestamptz NOT NULL DEFAULT now()
);
```

### Resolved: Session-Start Retrieval Is Unconditional
Every zero-prior-turn session triggers the memory-slot retrieval call, no cheap pre-check gate. Simpler, and never misses a case where retrieved memory would have helped, at the accepted cost of a wasted call on trivial first messages ("hi," "what's 2+2"). Revisit only if real usage data shows this cost matters in practice — consistent with this project's standing discipline of not building a gating mechanism ahead of evidence it's needed.

### Open Questions / To Design
- **Topic drift within one very long-lived session** — whether trusting aging-based deprioritization (older content sinks further into the DAG) is sufficient, or whether something more explicit is eventually needed. Deliberately left untouched — session_key is a channel identity, not a topic marker (`sessions.session_key` example: `agent:main:discord:guild:123`), so this is a real risk, just not one with evidence yet of needing a dedicated mechanism.
- **The deferred cheap Tier-1 salience pre-filter** (see "Retrieval Is Event-Triggered" above) — explicitly parked, not designed this pass.
- **Soft/hard threshold numeric values** — not sized yet, same treatment as every other numeric threshold in this project (heartbeat intervals, HPA, `max_iterations`): a placeholder now, tuned once real usage data exists.
- **Sequencing with sibling components** — this doc references `components/memory-slot.md`'s typed retrieval and `components/tool-registry.md`'s `search_memory`/`lcm_expand`-equivalent tools, neither of which has landed its own resolved design yet.

### Notes Log
- 2026-08-22: Fixed a real inconsistency surfaced while revising `memory-slot.md` to drop its generic backend-interface framing (that doc is now explicitly opinionated to agent-brain, not a pluggable abstraction): the LCM mapping table here still claimed `lcm_expand`'s subagent-only restriction applied, which `memory-slot.md` had already superseded (both `search`/`expand` unrestricted). Corrected to match. This doc's own LCM adoption was already stated as direct, not hedged as swappable, so no equivalent pivot was needed here — just this one stale cross-reference.
- 2026-08-17 (later): Resolved summary storage as a dedicated `context_summaries` table (leaf/condensed rows, `covers` array pointing at messages or child summaries), not a reuse of `messages`. Resolved session-start retrieval as unconditional — no cheap pre-check gate, simplicity and never-miss preferred over the wasted-call cost on trivial first messages, revisit only with real usage evidence.
- 2026-08-16: Introduced as a scaffold, one of five components (context-slot, memory-slot, model-registry, tool-registry, budget-guardrails) identified while discussing how to make the harness robust for genuinely complex, multi-step tasks. Grounded in `llm.py`'s unbounded replay and `compress_context.py`'s no-op stub — not yet designed.
- 2026-08-17: Resolved scope (whole session, not one turn — cross-turn continuity turned out not to exist at all today, so this closes a real gap, not scope creep), the seven core duties (assembly, tool-output bounding, compression trigger/execution, threshold sizing, cache stability, losslessness, scope boundary), retrieval as event-triggered rather than continuous (rejected on cache-stability grounds; a cheap non-LLM pre-filter explicitly deferred, not designed), a tiered system prompt (small static core identity vs. typed, selectively-retrieved situational content), and adopted LCM (Ehrlich & Blackman, Voltropy, Feb 2026 — "LCM: Lossless Context Management," benchmarked against Claude Code on OOLONG) as the concrete mechanism for nearly all of the above, evaluated against and preferred over three other reference points considered first (Karpathy's "LLM Wiki" gist, OpenKB, PageIndex — all found to be durable/cross-session knowledge-base tooling, better fits for `components/memory-slot.md` than this component). Storage schema for the summary DAG remains the single biggest open gap.
