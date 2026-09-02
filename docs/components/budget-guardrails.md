# Component: Budget & Guardrails
## (Turn Metrics + Rule-Driven Controlled Stop)

> STATUS: IN PROGRESS — metrics export (visibility only) resolved. The rule-driven controlled-stop half of this component's own subtitle is explicitly deferred — see "Future Scope."

### Role (one line)
Produces the real, observable usage metrics a turn accumulates (tokens, cost, iteration count, retries, wall-clock, per-tool spend), and evaluates configurable rules/conditions against them to force a controlled stop — replacing today's four hardcoded constants with something that's both measurable and tunable.

### Why this exists (recap)
`components/temporal-workflow.md`'s "Resolved: Stop-Condition Logic" and "...Default Values" sections already establish *where* stopping logic lives (pure inline workflow checks against already-recorded state) and today's actual thresholds: `max_iterations = 20`, `max_retries = 5` (turn-level, distinct from the per-activity retry ceiling), and a token/cost budget described as *"a generously high placeholder ceiling... tuned down later once real cost data exists."* Those are literal Go constants in `workflows/internal/workflow/turn.go` — not metrics anyone can observe outside a log line, and not rules anyone can configure per tenant or per session. This component is explicitly split from the context slot (`components/context-slot.md`) rather than folded into it: the context slot's job is curating *what the model sees*; this component's job is deciding, from real metrics, *whether the turn should keep going at all*.

### Responsibilities (from architecture)
- Define what metrics get tracked per turn (and rolled up per session/tenant): input/output tokens, estimated cost, iteration count, retry count, wall-clock duration, per-tool-call count and cost.
- Decide where these metrics are recorded durably — likely a Postgres table or columns (ties to `components/state-layer.md`) — versus what stays cheap-enough-to-check pure inline workflow logic, preserving the determinism-boundary reasoning already established in `temporal-workflow.md`.
- Define the rule/condition language for a "controlled stop" — today it's four fixed thresholds; a real version presumably needs configurable, possibly per-tenant or per-session, rules.
- Preserve the existing invariant that any stop is a clean, non-error observation: *"the turn completes with whatever partial result it has, not a hard failure"* (`temporal-workflow.md`) — a controlled stop is not a crash path, and this component shouldn't change that.

### Resolved: Metrics Export (Current Scope)
Scoped narrowly and deliberately: **visibility only** — token consumption, call counts, latency, iteration/retry counts, exported as real metrics an external observability system (Grafana, via Prometheus scraping) can visualize. Not the rule-driven controlled stop — that's deferred, see "Future Scope" below. Nothing here changes `turn.go`'s existing four hardcoded stop constants.

**Mechanism: Temporal's own built-in metrics handler on both SDKs, not a bespoke solution.** Both the Go and Python SDKs provide one, specifically designed to be safe to call from workflow code (replay-safe — doesn't double-count on replay, and doesn't create a command in workflow history the way `ExecuteActivity` does, so it needs no `GetVersion` gate under the determinism convention already resolved in `components/temporal-workflow.md`). Emitted at points where the data is already computed — no new activities, no new Postgres writes:

- **Go (`loop-worker`, workflow-side, `workflow.GetMetricsHandler(ctx)`)**:
  - `model_call_tokens_total` (counter, input/output, incremented each `ModelCall` step from `Usage`)
  - `turn_iterations_total`, `turn_retries_total` (from the loop's existing counters)
  - `turn_stop_reason_total` (counter, labeled by `stop_reason`, emitted once at turn completion)
  - **All labeled with `namespace`** (tenant), via `workflow.GetInfo(ctx).Namespace` — already an established, legitimate deterministic value elsewhere in this codebase. Required, not optional: `loop-worker` is shared across every tenant's namespace from one process, so an unlabeled metric collapses every tenant's turns into one undifferentiated number.

- **Python (`tenant-worker`, activity-side, the SDK's activity metric meter)** — for things the workflow doesn't directly see:
  - `model_call_latency_seconds` (histogram, real provider round-trip time)
  - `tool_call_total` (counter, labeled by `tool_name`, `status`)
  - `tool_call_latency_seconds` (histogram, labeled by `tool_name`)

**Token counts are available now; dollar cost is not — kept as separate, honest scope.** Cost requires a per-model `$`/token table, which `components/model-registry.md` was always meant to own but never actually built. Token-count metrics ship without waiting on that; cost-based metrics are a later addition once that dependency resolves, not blocked-and-silent.

**Export: plain scrape, no ServiceMonitor.** Each worker process exposes its own `/metrics` HTTP endpoint (Prometheus exposition format) — `loop-worker` wires the Go SDK's metrics handler to a Prometheus registry and starts a listener (`workflows/cmd/loop-worker/main.go`); `tenant-worker` does the equivalent on the Python side (`activities/activities/tenant_worker.py`). Both charts need plain `prometheus.io/scrape`/`prometheus.io/port`/`prometheus.io/path` pod annotations added — `agent-harness-shared`'s `loop-worker-deployment.yaml` and `agent-harness-tenant`'s `tenant-worker-deployment.yaml` — no `ServiceMonitor` CRD, confirmed not needed for this cluster's setup.

**Extended to the Gateway process, 2026-08-26** — not this component's own turn-level metrics, but the same mechanism, added when real voice-latency numbers were needed (`docs/components/gateway/discord-voice.md`'s Notes Log has the full detail: `voice_first_audio_latency_seconds`, `voice_chunk_gap_seconds`, `voice_tts_ttfb_seconds`, `voice_chunk_signal_gap_seconds`). The Gateway (`workflows/cmd/gateway/main.go`) had no metrics exposition at all before this — it now dials its Temporal client with the identical `newMetricsHandler` construction loop-worker already used (duplicated, not shared — three independent binaries, no existing common package worth introducing for one function), scraped the same `prometheus.io/*` annotation way (`gateway.metrics.*` in `agent-harness-tenant`'s `values.yaml`, `enabled: true` by default at port 9090).

### Resolved: Pipeline-Phase Visibility (2026-09-02)

A Grafana pass on a full scenario-suite run showed the pre-LLM pipeline
(`components/request-pipeline.md`) is ~60% of turn wall-time — `RoutingWorkflow`
alone averaged 7.4s, and inside it `ComposeSkill` is ~7.2s (a medium-tier merge
call, `max_tokens=1100`). Three gaps fixed:

- **Per-activity latency was there all along, under the wrong name.** The
  Python SDK core emits `temporal_activity_execution_latency` (histogram, unit
  = **milliseconds**, label `activity_type`) for *every* activity — that's the
  metric to query for `ClassifyRequest` / `ComposeSkill` / `MemoryRetrieve` /
  … timing. The `..._seconds` variant is emitted only by the Gateway (Go, which
  sets `durations_as_seconds=true`), so a query for
  `temporal_activity_execution_latency_seconds{activity_type="ComposeSkill"}`
  silently returns nothing. No code change — a query/dashboard note.
- **The three hand-rolled `*_latency_seconds` histograms had unusable
  buckets.** They record seconds but kept Temporal core's default
  (millisecond-oriented) boundaries, so every real value fell in the first
  bucket and every percentile read the same number. Fixed with
  `PrometheusConfig(histogram_bucket_overrides=…)` in `tenant_worker.py` —
  seconds-appropriate boundaries for `classify_request_latency_seconds`,
  `model_call_latency_seconds`, `tool_call_latency_seconds` (the list and the
  boundaries live in `activities/metrics.py`).
- **Semantic outcome was invisible.** The SDK metric shows an activity
  succeeded, not *what it decided*. Added one counter per pre-LLM activity via
  the `observe_outcome` decorator (`activities/metrics.py`):
  `compose_skill_total` / `memory_retrieve_total` / `tool_discover_total` /
  `skill_discover_total` (`outcome` = `ok|empty|error`), `open_episode_total`
  (`outcome` = `opened|attached|superseded|none`), plus
  `compose_skill_merge_total` (`result` = `merged|passthrough|no_provider|call_failed|unparsed`)
  so a degraded compose is distinguishable from a real merge. `classify`'s
  existing latency histogram now also carries the `intent`/`fallback`
  attributes its counter already had, so classify latency can be sliced by
  whether it fell back (a fallback both costs latency *and* forces the
  Deliberate lane).

### Future Scope: Rule-Driven Controlled Stop
Deliberately not addressed in this pass — parked, not designed:
- Rule expressiveness: fixed named thresholds (today's shape, just made configurable) vs. a genuine rule engine evaluating arbitrary boolean conditions over the tracked metrics.
- Where configuration lives — per-tenant Postgres row, per-session override, org-wide default — the same "where does config live" question also open in `components/model-registry.md` and `components/tool-registry.md`; worth resolving consistently across all three rather than three different answers.
- Dependency on `components/model-registry.md`: real cost tracking needs a per-model `$`/token table, which the model registry is the natural owner of — this component should consume that data, not maintain its own copy. Still genuinely unresolved on the model-registry side too, not just here.
- Relationship to `components/context-slot.md`: the context slot's compression gate already reads `cumulativeTokens` today (`temporal-workflow.md`). Should this component become the sole owner of that metric going forward, with the context slot as a consumer rather than each side maintaining its own count?
- Whether a stop can ever be non-final — e.g. a soft warning before a hard stop, or a human-in-the-loop check — which raises the same "does this need a new interrupt-like mechanism" question already flagged for permission gating (`future-work.md` §4, `components/tool-registry.md`).

### Notes Log
- 2026-08-16: Introduced as a scaffold, split out as its own component (rather than folded into `components/context-slot.md`) at the user's explicit direction — the focus is producing real metrics first, then a rule-driven controlled stop on top of them, distinct enough from context curation to warrant its own design surface. Grounded in `turn.go`'s current hardcoded stop-condition constants and `temporal-workflow.md`'s existing resolved stop-condition logic, audited the same day — not yet designed.
- 2026-09-02: Pipeline-phase visibility pass (see "Resolved" section above) — `activities/activities/metrics.py` (new: `observe_outcome` decorator + the seconds-histogram bucket-override list), `tenant_worker.py` (`histogram_bucket_overrides` on `PrometheusConfig`), `observe_outcome` applied to `ComposeSkill`/`MemoryRetrieve`/`SkillDiscover`/`ToolDiscover`/`OpenEpisode`, `compose_skill_merge_total` in `retrieval/compose.py`, `intent`/`fallback` attributes added to `classify_request_latency_seconds`. Latency itself is unchanged — this pass only makes where the ~9s pre-LLM cost goes measurable; the cuts (ComposeSkill retry/timeout, classify fallback root-cause, serial critical path) are the follow-up.
- 2026-08-22: At the user's explicit direction, scoped this pass to metrics export only (visibility for an external observability system), deferring the rule-driven controlled-stop half entirely to "Future Scope." Resolved the export mechanism as Temporal's own built-in per-SDK metrics handlers (replay-safe, no new activities/Postgres writes, no `GetVersion` gate needed), the concrete metric set on both the Go workflow side and Python activity side, the required per-tenant `namespace` label (since `loop-worker` is shared across tenants), and plain Prometheus scrape annotations (no `ServiceMonitor`) on both charts' worker Deployments.
