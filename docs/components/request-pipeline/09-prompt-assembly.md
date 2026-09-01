# Request Pipeline — Step 9: Prompt Assembly

> STATUS: BUILT (2026-09-01). New `activities/activities/prompt.py` — the
> section model, ordering, and budget arbitration. `llm.build_conversation` is
> now a thin call-through kept as `model_call.py`'s stable call site.
> `model_call.py` resolves the model tier (and its `context_window`) before
> assembling, so assembly can bound enrichment against it. No Go changes — this
> runs entirely inside the `ModelCall` activity, every call.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Reads: [`04-memory-retrieval.md`](04-memory-retrieval.md) (`kind='memory'`),
> [`../skill-subsystem.md`](../skill-subsystem.md) (`kind='composed'`),
> [`07-tool-discovery.md`](07-tool-discovery.md) (`kind='tool'`),
> [`08-planning.md`](08-planning.md) (`turn_plan`).
> Delegates to: `docs/components/context-slot.md`'s `lcm.assemble` for the
> summary DAG + verbatim window.

### Role

Turn everything the request pipeline staged for a turn into **one ordered,
budget-bounded conversation** — a single place that owns section order and what
gets dropped when it doesn't all fit, instead of the ad-hoc stack of
`conversation.insert(1, ...)` calls this used to be.

### Not a new activity

Prompt assembly runs **inside `ModelCall`**, every call — it was never a
separate pipeline phase with its own Temporal dispatch, and doesn't become one
here. `llm.build_conversation(conn, turn_id, system_prompt, context_window)`
is kept as the one call site `model_call.py` already uses; it now just forwards
to `prompt.assemble`. Splitting the *implementation* into `prompt.py` is a
clean-code move (this belongs next to `plan.py` as its own concern, not folded
into `llm.py`'s provider-neutral constants), not an architectural one.

### The sections, outer → inner

Order is **most task-specific first, live conversation last** — so the
conversation dominates when the model reads it (memory-slot.md's "Resolved:
Staleness Is Handled by Placement"). Each is a single `role: "system"` block,
inserted right after the system prompt, ahead of `lcm.assemble`'s own summary
DAG + verbatim window.

| # | Section | Source | Droppable |
|---|---|---|---|
| 1 | System prompt | `DEFAULT_SYSTEM_PROMPT` / session override | never |
| 2 | Composed skill | `turn_retrieval` `kind='composed'` | never |
| 3 | Plan progress | `turn_plan` (step 8) | never |
| 4 | **Capabilities** | `turn_retrieval` `kind='tool'` | yes — shed 1st |
| 5 | Long-term memory | `turn_retrieval` `kind='memory'` | yes — shed 2nd |
| 6 | Summary DAG | `lcm.assemble` | (context-slot.md's own concern) |
| 7 | Verbatim window | `lcm.assemble` | (the compression gate's job) |

Sections 6–7 aren't touched here — `lcm.assemble` already self-manages their
size (capped verbatim window, compressed summary DAG), and shrinking the live
conversation under pressure is `context-slot.md`'s compression gate, a
different mechanism with a different trigger (a hard token threshold *after*
a `ModelCall*returns*, not an assembly-time budget).

Any section with nothing to say is simply absent — no empty blocks.

### The capabilities section (new)

`ToolDiscover` (step 7) has staged `kind='tool'` rows since it was built, but
nothing ever read them into the prompt — only `ComposeSkill` used them, to bind
a composed procedure's abstract `tool_ref`s. A turn with no composed skill (a
`question`, or a `task` whose `SkillDiscover` came up empty) got tool discovery
for nothing. This section closes that gap with a plain **hint**, not a schema
change:

```
These environment tools look relevant to your task — use call_tool to invoke one:
- github/create_pr — open a pull request
- maps/geocode — resolve an address to coordinates
```

The model's actual callable tool set (`llm.tools_schema_for`) is unchanged —
this just saves an obvious `search_tools` round-trip for the tools discovery
already found relevant. Injecting discovered tools into the live schema itself
(so the model could call them without `call_tool`) was considered and rejected
as a separate, bigger change — `07-tool-discovery.md` flagged this same
trade-off as open and predicted this resolution.

### Budget arbitration

```
budget = context_window * ENRICHMENT_BUDGET_FRACTION     # 0.25, placeholder
enrichment_total = sum(section.tokens for section in [composed, plan, capabilities, memory] if present)

for name in ("capabilities", "memory"):     # shed order — least task-critical first
    if enrichment_total <= budget: break
    drop `name` if present; enrichment_total -= its tokens
```

`context_window == 0` (unresolved model tier, or the fixture path, which never
calls this at all) means **no budget info — nothing is ever shed**, same
"degrade to unbounded" posture `model_call.py` already gives a zero
`context_window` elsewhere (turn.go falls back to its own static thresholds).

**Composed skill and plan progress are never shed.** They *are* the task — a
call without them is barely better-informed than one with an empty prompt, so
there's little point sending it at all. If they alone exceed the budget (a
tiny-context tier with a large composed procedure), assembly logs it and sends
them anyway rather than silently degrading the one thing that matters; the
compression gate and the model's own tier-escalation-on-retry are the real
backstops for a call that's genuinely too big.

**Why whole-section shedding, not row-level trimming.** Memory is already
budget-capped at write time (`retrieval/memory.py`'s own `_TOKEN_BUDGET=1500`,
lowest-score rows dropped first) — by the time it reaches `turn_retrieval` it's
already small. At the ceiling (composed ~1100 + plan ~300 + capabilities ~300 +
memory ~1500 tokens ≈ 3200), this only matters for a context window under
~13k — a genuinely tiny tier. Row-level trimming at assembly time was
considered and skipped as complexity a real deployment isn't hitting yet;
revisit if a small-context tier is actually in use and the logs show shedding
whole sections routinely.

### Data flow

```
ModelCall
  ├─ resolve hint_tier → model_config (context_window)   ← moved before assembly (was after)
  ├─ read session_row → system_prompt
  └─ prompt.assemble(conn, turn_id, system_prompt, context_window)
       ├─ lcm.assemble(conn, session_key, system_prompt)      → conversation, base_tokens
       ├─ read + render composed / plan / capabilities / memory
       ├─ shed under budget pressure (capabilities, then memory)
       └─ conversation.insert(1, …) in reverse section order  → conversation, context_tokens
```

`context_window` had to move earlier in `model_call.py` (it used to resolve
*after* `build_conversation`) — a small, behavior-preserving reorder; nothing
downstream depended on the old order.

### Degradation

- No composed skill / no plan / no discovered tools / no memory → that section
  is simply absent. A turn with nothing staged at all degrades exactly to
  `lcm.assemble`'s own output — today's behavior, unchanged.
- `context_window` unknown → no shedding, same as before this phase existed.
- Any section read failing would propagate out of `ModelCall` like any other
  activity error — no per-section try/except here, matching this codebase's
  posture that a `ModelCall` failure is Temporal's retry policy's job, not a
  swallowed-and-degraded one (unlike the *retrieval* phase's activities, which
  are best-effort because they run outside the load-bearing model call).

### Open Questions

- **`ENRICHMENT_BUDGET_FRACTION = 0.25`** — placeholder, numeric-tuning-deferred
  like every other threshold in this project.
- **Row-level memory trimming at assembly time** — deferred, see "Why
  whole-section shedding" above.
- **Capabilities section content cap** — `ToolDiscover` already caps at
  `top_k=10`; whether the rendered hint block needs its own cap independent of
  the whole-section shed is unmeasured.
