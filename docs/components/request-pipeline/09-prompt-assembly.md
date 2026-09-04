# Request Pipeline — Step 9: Prompt Assembly

> STATUS: BUILT. `activities/activities/prompt.py` — the section model,
> ordering, and budget arbitration. `llm.build_conversation(conn, turn_id,
> plan_id, system_prompt, context_window)` is a thin call-through kept as
> `model_call.py`'s stable call site. Runs entirely inside the `ModelCall`
> activity, every call. `model_call.py` resolves the model tier (and its
> `context_window`) first, so assembly bounds enrichment against it.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Reads `turn_retrieval`: `kind='memory'` / `kind='tool'` rows keyed
> `owner_id = turn_id` ([`04`](04-memory-retrieval.md) / [`07`](07-tool-discovery.md)),
> `kind='skill'` rows keyed `owner_id = plan_id` ([`05`](05-skill-discovery.md)),
> plus PLAN.md ([`08`](08-planning.md)). Delegates the summary DAG + verbatim
> window to `context-slot.md`'s `lcm.assemble`.
>
> **REVISION (2026-09-04 — DESIGNED, not built; `tool-registry.md` "Resolved:
> Three-Layer Tool Taxonomy & Per-Task Resolution").** The **capabilities**
> section is no longer a prompt section for reasoning / checkpoint turns —
> `ToolDiscover`'s rows are bound as callable schemas by `tools_schema_for`
> instead. It survives **only for the planning turn**, rendered as a reference
> capability catalog (that turn invokes nothing). So on every non-planning turn
> the shed list is just `("memory",)`, and section 4 below is planning-only.

### Role

Turn everything the request pipeline staged for a turn into **one ordered,
budget-bounded conversation** — a single place that owns section order and what
gets dropped when it doesn't all fit, instead of the ad-hoc stack of
`conversation.insert(1, ...)` calls this used to be.

### Not a new activity

Prompt assembly runs **inside `ModelCall`**, every call — it was never a
separate pipeline phase with its own Temporal dispatch, and doesn't become one
here. `llm.build_conversation(conn, turn_id, plan_id, system_prompt,
context_window)` is kept as the one call site `model_call.py` already uses; it
forwards to `prompt.assemble`. Splitting the *implementation* into `prompt.py`
is a clean-code move (this belongs next to `plan.py` as its own concern, not
folded into `llm.py`'s provider-neutral constants), not an architectural one.

### The sections, outer → inner

Order is **most task-specific first, live conversation last** — so the
conversation dominates when the model reads it (memory-slot.md's "Resolved:
Staleness Is Handled by Placement"). Each is a single `role: "system"` block,
inserted right after the system prompt, ahead of `lcm.assemble`'s own summary
DAG + verbatim window.

| # | Section | Source | Droppable |
|---|---|---|---|
| 1 | System prompt | `DEFAULT_SYSTEM_PROMPT` / session override / `PLANNING_SYSTEM_PROMPT` | never |
| 2 | Skills | `turn_retrieval` `kind='skill'` (`owner_id = plan_id`) — the full rendered procedures `SkillDiscover` staged | never |
| 3 | Plan | PLAN.md `render_block` — the checkpoint this turn runs + surrounding plan | never |
| 4 | Capabilities catalog (**planning turn only** since 2026-09-04) | `turn_retrieval` `kind='tool'` (`owner_id = turn_id`) | yes — shed 1st |
| 5 | Long-term memory | `turn_retrieval` `kind='memory'` (`owner_id = turn_id`) | yes — shed 2nd (1st on non-planning turns) |
| 6 | Summary DAG | `lcm.assemble` | (context-slot.md's own concern) |
| 7 | Verbatim window | `lcm.assemble` | (the compression gate's job) |

Sections 6–7 aren't touched here — `lcm.assemble` already self-manages their
size (capped verbatim window, compressed summary DAG), and shrinking the live
conversation under pressure is `context-slot.md`'s compression gate, a
different mechanism with a different trigger (a hard token threshold *after*
a `ModelCall*returns*, not an assembly-time budget).

Any section with nothing to say is simply absent — no empty blocks.

### The capabilities section (planning turn only, since 2026-09-04)

Originally, `ToolDiscover`'s `kind='tool'` rows rendered as a plain hint on
every task turn, and the "inject into the live schema" alternative was
deferred. The 2026-09-04 revision (`tool-registry.md`) takes that alternative:
for reasoning / checkpoint turns the rows are now bound as **callable function
schemas** by `tools_schema_for`, so there is nothing for this section to add —
it is dropped for those turns.

It stays for the **planning turn**, which invokes nothing but needs to know
what's reachable to draft a good plan. Rendered as a reference catalog:

```
Capabilities available for this task (call them by name once execution begins):
- github/create_pr — open a pull request
- maps/geocode — resolve an address to coordinates
```

### Budget arbitration

```
budget = context_window * ENRICHMENT_BUDGET_FRACTION     # 0.25, placeholder
enrichment_total = sum(section.tokens for section in [skills, plan, capabilities, memory] if present)

# "capabilities" is only ever present on a planning turn (2026-09-04);
# on every other turn the shed list is effectively just ("memory",)
for name in ("capabilities", "memory"):     # shed order — least task-critical first
    if enrichment_total <= budget: break
    drop `name` if present; enrichment_total -= its tokens
```

`context_window == 0` (unresolved model tier, or the fixture path, which never
calls this at all) means **no budget info — nothing is ever shed**, same
"degrade to unbounded" posture `model_call.py` already gives a zero
`context_window` elsewhere (turn.go falls back to its own static thresholds).

**Skills and the plan ledger are never shed.** They *are* the task — a call
without them is barely better-informed than one with an empty prompt. If they
alone exceed the budget (a tiny-context tier with a large retrieved procedure),
assembly logs it and sends them anyway rather than silently degrading the one
thing that matters; the compression gate and the model's own
tier-escalation-on-retry are the real backstops for a call that's genuinely too
big.

**Why whole-section shedding, not row-level trimming.** Memory is already
budget-capped at write time (`retrieval/memory.py`'s own `_TOKEN_BUDGET=1500`,
lowest-score rows dropped first). Row-level trimming at assembly time was
considered and skipped as complexity a real deployment isn't hitting yet;
revisit if a small-context tier is in use and the logs show whole sections shed
routinely.

### Data flow

```
ModelCall
  ├─ resolve hint_tier → model_config (context_window)   ← before assembly
  ├─ read session_row → system_prompt (or PLANNING_SYSTEM_PROMPT in planning mode)
  └─ prompt.assemble(conn, turn_id, plan_id, system_prompt, context_window)
       ├─ lcm.assemble(conn, session_key, system_prompt)      → conversation, base_tokens
       ├─ _staged_texts: one turn_retrieval query (memory/tool by turn_id, skill by plan_id)
       │  + plan.read(plan_id) → render_block
       ├─ shed under budget pressure (capabilities [planning turn only], then memory)
       └─ conversation.insert(1, …) in reverse section order  → conversation, context_tokens
```

### Degradation

- No retrieved skill / no plan / no discovered tools / no memory → that section
  is simply absent. A turn with nothing staged at all degrades exactly to
  `lcm.assemble`'s own output.
- `context_window` unknown → no shedding, same as before this phase existed.
- Any section read failing would propagate out of `ModelCall` like any other
  activity error — no per-section try/except here, matching this codebase's
  posture that a `ModelCall` failure is Temporal's retry policy's job, not a
  swallowed-and-degraded one (unlike the *retrieval* phase's activities, which
  are best-effort because they run outside the load-bearing model call).

### DB access

Assembly runs on **every real `ModelCall`** (the fixture path in `model_call.py`
skips it entirely — which is why the `real-assembly` scenario forces the real
path). Reads, per call:

- `lcm.assemble`: session summaries (1), the session-wide message list (1), and
  — **batched** — every window assistant message's `tool_calls` in one
  `WHERE message_id = ANY(...)` (`migration 019` added `tool_calls(message_id)`).
- `prompt.assemble`: skills / discovered tools / long-term memory in **one**
  `turn_retrieval` query split by `kind`. PLAN.md is a PV file read, not a query.

`prompt_assemble_latency_seconds` (histogram, seconds — `metrics.SECONDS_LATENCY_METRICS`)
is recorded around the `build_conversation` call in `model_call.py`. Real path
only, by construction.

### Open Questions

- **`ENRICHMENT_BUDGET_FRACTION = 0.25`** — placeholder, numeric-tuning-deferred
  like every other threshold in this project.
- **Row-level memory trimming at assembly time** — deferred, see "Why
  whole-section shedding" above.
- **Capabilities → callable-schema binding (2026-09-04, DESIGNED)** — how many
  of `ToolDiscover`'s `top_k=10` rows `tools_schema_for` should actually bind as
  callable schemas (the "top few"), and how aggressively to trim each
  `input_schema`, is unmeasured — some MCP schemas are large enough that binding
  all ten would cost more than the old hint block. Start conservative (3–5,
  name + one-line + required params) and widen on evidence.
- **Planning-turn catalog cap** — the planning turn's reference catalog renders
  from `content` (one line each), so it's cheap; whether it needs a cap
  independent of the whole-section shed is unmeasured.
