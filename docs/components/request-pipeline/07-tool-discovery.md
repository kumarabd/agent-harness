# Request Pipeline — Step 7: Tool Discovery

> STATUS: BUILT — `activities/activities/retrieval/tools.py`. Calls the ctx-free
> `tools.discover_tools` (extracted from `search_tools`), renders
> `{server}/{tool} — description` lines, stages `kind='tool'` rows (keyed on the
> current turn's id, `turn_retrieval.owner_id`) with the `input_schema` in
> `metadata`. Runs **per turn**; `prompt.assemble` reads them back as the
> capabilities-hint block. The model binds tools at execution time from that
> block (there is no skill-composition step to pre-bind them).
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Orchestrated by [`03-routing.md`](03-routing.md)'s `RoutingWorkflow`.
> Backend: [`../tool-registry.md`](../tool-registry.md) (`discover_tools`).

### Role

Resolve which of this tenant's registered capabilities are **relevant to the
task**, so the turn's prompt carries a task-scoped tool hint instead of only the
full static schema — and so the harness knows whether a needed capability exists
at all ("is Grafana connected?").

### Activation

Runs when `intent == task`. Skipped for `conversational` / `question` / `meta` —
a Q&A or chat turn doesn't need capability resolution, and the model keeps its
always-on tools (`shell_exec`, `call_tool`, `search_tools`) regardless.

### `ToolDiscover` activity

- **Runs on:** tenant-worker (`MCP_HUB_URL`, `EMBEDDING_*`).
- **Input:** `{turn_id, retrieval_query, entities}` — both from step 2's
  `TaskRepresentation`, passed straight in by `RoutingWorkflow`.
- **Query** (`_query`) — `retrieval_query`, sharpened with any `entity` not
  already a substring of it (so "is Grafana connected?" keeps "Grafana" in the
  search text).
- **Mechanism:** `tools.discover_tools(query, top_k=10)` — the ctx-free core
  extracted from the model-facing `search_tools` tool. Fans out to mcp-hub's
  semantic search **and** the in-process shell-hub index, splits `top_k` (mcp-hub
  the curated primary, remainder on an odd split), returns one combined ranked
  list of `{server, tool, description, input_schema[, score]}`. An unregistered
  backend is simply absent — "not connected" falls out for free.
- **Output:** one `turn_retrieval` row per deduped `(server, tool)` —
  `kind='tool'`, `seq` = rank, `content` = `"{server}/{tool} — {description}"`,
  `score` if present, `metadata = {server, tool, input_schema}` (the composer /
  planner need the schema to bind and construct calls).
- **Status:** `ok` (≥ 1 row) | `empty` (empty query, or both backends
  unconfigured, or no results) | `error` (genuine call failure — **raised**, no
  in-activity retry; `RoutingWorkflow`'s `RetryPolicy` handles it) | `timed_out`.

### Advisory, not restrictive

Tool discovery output **pre-warms** the planner and the skill composer — it does
not restrict the model. The reason-act loop still offers the full always-on tool
set, and the model can call `search_tools` itself mid-turn to discover more. A
discovery miss therefore costs a slightly less-informed plan, never a blocked
capability.

### Consumers

- **Prompt assembly (step 9)** — the only consumer. Keeps the full tool schema
  and surfaces the discovered subset as a plain hint block (the "capabilities"
  section, `09-prompt-assembly.md`) rather than narrowing what the model can
  call. Narrowing was rejected — it risks hiding a tool the model would have
  known to ask for. The planning turn and each checkpoint turn read the block
  and pick / bind tools themselves.

### Relationship to `tool-registry.md`

No change to the two-tier architecture, mcp-hub adoption, or shell-hub. The one
code change is a refactor: `search_tools`'s body moved into a ctx-free
`discover_tools(query, top_k)`; the model-facing `search_tools` tool is now a
one-line wrapper (`{"results": await discover_tools(...)}`), unchanged in
behavior. Both it and this phase call the same primitive. The model-initiated
path stays.

### Notes Log

- 2026-09-01: **`discover_tools` hardened after a live failure.** A real
  message (a pasted markdown process doc, full of `word:` and quotes) went
  through as the `retrieval_query` verbatim — `ClassifyRequest` had degraded
  to the raw message on a provider 503 — and blew up `shell_hub.search`'s
  zvec FTS lane (`FTS query parse failed: field-prefixed queries are not
  supported`). That exception propagated out of `discover_tools` and threw
  away the mcp-hub results too, failing `ToolDiscover` on every retry. Two
  fixes: (1) `shell_hub.search` now reduces the query to a lowercase
  alphanumeric token bag (`_fts_safe`, capped at 32) for the FTS lane only —
  the vector lane still gets the raw query, and FTS here is just an
  RRF-fused keyword boost, not load-bearing; an empty result skips the FTS
  `Query` entirely. (2) `discover_tools` isolates the two sources —
  shell-hub failures are logged and skipped (deterministic, not worth a
  retry, never worth losing mcp-hub over); mcp-hub failures still propagate
  so `RoutingWorkflow`'s retry can recover a transient outage.

### Open Questions

- Top-k value (`_TOP_K = 10`) — deferred, numeric-tuning discipline.
- mcp-hub's result score field — `_score` probes `score` / `similarity` /
  `rrf_score` defensively; confirm the real shape.
- Whether **mcp-hub's** own `search_tools` needs the same query scrubbing —
  it didn't error in the live incident (it's a remote semantic-search
  service, not local zvec), but unconfirmed for adversarial input.
