# Request Pipeline — Step 7: Tool Discovery

> STATUS: IMPLEMENTED — `activities/activities/retrieval/tools.py`. Calls the
> new ctx-free `tools.discover_tools` (extracted from `search_tools`), renders
> `{server}/{tool} — description` lines, stages `kind='tool'` rows with the
> `input_schema` in `metadata`.
>
> Parent: [`../request-pipeline.md`](../request-pipeline.md).
> Orchestrated by [`03-routing.md`](03-routing.md)'s `RoutingWorkflow`.
> Backend: [`../tool-registry.md`](../tool-registry.md) (`discover_tools`).

### Role

Resolve which of this tenant's registered capabilities are **relevant to the
task**, so the planner (step 8) and skill composition (step 6) work with a
task-scoped tool set instead of the full static schema — and so the harness
knows whether a needed capability exists at all ("is Grafana connected?").

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

- **Skill composition (step 6)** — resolves the skeleton's *abstract* tool
  references ("a git-hosting tool", "a metrics-query tool") to concrete
  `{server, tool}` pairs against this list, and the checkpoint ledger it emits
  (step 8) inherits those bindings. This is why `ComposeSkill` waits for
  `ToolDiscover` to settle.
- **Prompt assembly (step 9)** — resolved: kept the full tool schema, and
  surfaces the discovered subset as a plain hint block (a "capabilities"
  section, `09-prompt-assembly.md`) rather than narrowing what the model can
  call. Narrowing was rejected — it risks hiding a tool the model would have
  known to ask for.

### Relationship to `tool-registry.md`

No change to the two-tier architecture, mcp-hub adoption, or shell-hub. The one
code change is a refactor: `search_tools`'s body moved into a ctx-free
`discover_tools(query, top_k)`; the model-facing `search_tools` tool is now a
one-line wrapper (`{"results": await discover_tools(...)}`), unchanged in
behavior. Both it and this phase call the same primitive. The model-initiated
path stays.

### Open Questions

- Top-k value (`_TOP_K = 10`) — deferred, numeric-tuning discipline.
- mcp-hub's result score field — `_score` probes `score` / `similarity` /
  `rrf_score` defensively; confirm the real shape.
