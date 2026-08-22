# Component: Model Registry

> STATUS: IN PROGRESS — registry structure, selection mechanism, and escalate-on-retry resolved. Storage location and multi-provider abstraction still open.

### Role (one line)
Lets the harness select an appropriate model per task at runtime — which model to call, based on what the turn/subagent/step actually needs — replacing today's single model hardcoded into worker startup with no selection mechanism at all.

### Why this exists (recap)
`activities/activities/tenant_worker.py` constructs exactly **one** `AsyncOpenAI` client at process startup, from `PIONEER_API_KEY`/`PIONEER_BASE_URL`/`PIONEER_MODEL`/`PIONEER_MAX_TOKENS` env vars, and injects it once into `ModelCallActivity.__init__`. Every `ModelCall`, for every turn and every subagent at every recursion depth, uses that same one model. There is no mechanism today for, e.g., a subagent doing a narrow, cheap task to use a smaller/faster model while the top-level turn uses a stronger one, or for a tenant to configure more than a single model choice.

### Responsibilities (from architecture)
- Define what models are available: name, provider/base URL, context window size, cost (`$`/input token, `$`/output token), and capability tier.
- Decide selection: how `ModelCall` picks which model to use for a given turn/subagent/step — static per-tenant default vs. dynamic per-task choice.
- Expose per-model context-window size so the context slot (`components/context-slot.md`) can make its compression threshold model-aware, instead of the single hardcoded `budgetTokens` constant in `turn.go` today.
- Stay inside the reference-passing / tenant-isolation contract already established (`components/temporal-workflow.md`, `components/multi-tenancy.md`): model credentials remain tenant-worker-side only, never cross into the shared loop-worker.

### Resolved: Registry Structure — Modality × Tier
```
registry[modality][tier] -> model config
```
- `modality: language` — real, three tiers: `fast | medium | expert`.
- `modality: vision | audio | video` — **placeholders only for now**, no real dispatch logic. Nothing in the current toolset (`shell_exec` plus fixture stubs) calls any of these; the registry's shape accommodates them cheaply, but selection logic for them is explicitly deferred until a real consumer exists.

### Resolved: Selection Mechanism — Model Self-Declares the Next Step's Hint
`ModelCallOutput` gains a `next_step_hint: {modality, tier}` field — the model, as part of producing its current response, also declares what the *next* step needs. This rides along on output the model already has to produce, so it costs no extra call, unlike a separate classification hop. The hint threads into the following `ModelCallInput`; the **activity** (Python, tenant-worker) resolves it against the registry and picks the concrete model — the Go workflow passes the hint through as opaque data and never needs to interpret it, consistent with the tenant-isolation contract.

- **Bootstrapping**: the first `ModelCall` of a turn has no prior hint. Defaults to `{language, medium}`.
- **Applied every step, not damped.** The model re-declares a hint at every reasoning step, since step complexity genuinely varies step to step — a deliberate choice, not an oversight of the cache-prefix-stability concern named in `components/context-slot.md`. The actual cache cost is proportional to how often the declared tier *changes* between consecutive steps, not to how often it's declared — a run of several same-tier steps in a row stays cache-stable even though each one re-declares.

### Resolved: Escalate-on-Retry
When a `ModelCall` attempt fails (e.g. unparseable tool-call output from a fast-tier model), Temporal's own retry mechanism fires. The activity reads its own attempt number via `activity.GetInfo(ctx).Attempt` and escalates the tier by one step per attempt (`fast → medium → expert`), capped at `expert` — never escalates past the top tier. `RetryPolicy.MaximumAttempts` should be sized to match the number of language tiers (3) so the retry ladder and the escalation ladder line up, rather than being an arbitrary retry count picked independently.

### Open Questions / To Design
- Storage: a Postgres table (tenant-scoped, consistent with `components/state-layer.md`), a config file, or an extended env-var scheme — where does the registry actually live.
- Multi-provider support: is this OpenAI-compatible-only, matching today's `llm.py` assumption, or does it need to abstract across genuinely different provider APIs (different request/response shapes, not just different base URLs)?
- **Fallback beyond escalate-on-retry**: what happens if the top tier (`expert`) also fails — not addressed yet; presumably falls through to existing generic activity/turn-level error handling, not something specific designed here.
- Interaction with `components/budget-guardrails.md`: per-model cost feeds real cost tracking — this doc should own the pricing table, budget/guardrails should consume it, not duplicate it.
- Vision/audio/video dispatch logic — deferred until a real consumer exists; registry shape only for now.
### Notes Log
- 2026-08-16: Introduced as a scaffold, one of four (later five, with budget/guardrails split out separately) components identified while discussing how to make the harness robust for genuinely complex, multi-step tasks. Grounded in `tenant_worker.py`'s current single-client-at-startup construction, audited the same day — not yet designed.
- 2026-08-17: Resolved the registry structure (modality × tier, language real with three tiers, vision/audio/video placeholder-only), the selection mechanism (model self-declares the next step's hint at zero extra call cost, resolved activity-side, applied every step by deliberate choice rather than damped), and escalate-on-retry via Temporal's own attempt-count mechanism. Storage, multi-provider abstraction, and fallback-past-top-tier remain open.
- 2026-08-17 (later): Resolved `components/memory-slot.md`'s extraction-model dependency — extraction reuses the `fast` language tier directly, no separate named purpose/hint needed.
