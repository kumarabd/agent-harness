# Agent Loop Worker — First Implementation Slice

This is the first real implementation slice of the harness designed in `docs/`:
the **Session Coordinator** and **Turn Workflow** (`docs/components/session-coordinator.md`,
`docs/components/temporal-workflow.md`), proving out the resolved reason-act-observe
loop design end to end against a local Temporal dev server.

**Scope:** workflow mechanics only. Activities (ModelCall, ToolCall, Persist,
Deliver, CompressContext) are stubs — no real LLM, no Postgres, no gateway.
`ModelCall` returns a caller-supplied *scripted* sequence of responses instead of
calling a real model, so loop paths (stop conditions, subagent spawn, interrupt
handling) can be exercised deterministically.

**Language split:** the workflow layer (`workflows/`) is Go; the activity layer
(`activities/`) is Python. This is a standard, supported Temporal "polyglot"
pattern — the two worker processes only need to agree on a task queue name and
JSON-serializable activity input/output shapes, not a shared language.

## Prerequisites

- Go 1.21+
- Python 3.10+
- [Temporal CLI](https://docs.temporal.io/cli) (`temporal` on your `PATH`) — provides the local dev server

## One-time setup

```sh
# Go side
cd workflows
go mod tidy

# Python side
cd ../activities
python3 -m venv .venv
.venv/bin/pip install -e .
```

## Running it

Four terminals (or four background processes):

```sh
# 1. Local Temporal dev server
temporal server start-dev
# Web UI: http://localhost:8233

# 2. Go loop-worker
cd workflows
go run ./cmd/loop-worker

# 3. Python tenant-worker
cd activities
.venv/bin/python -m activities.tenant_worker

# 4. Starter CLI — submits a scripted scenario as if it were an inbound gateway message
cd workflows
go run ./cmd/starter -session my-session -scenario scenarios/happy-path.json
```

The starter does the one thing the real Gateway's inbound path does after dedup
(`docs/components/gateway.md`): `SignalWithStart` against the Session
Coordinator (workflow ID = session key).

## Scenarios (`workflows/scenarios/`)

Each scenario is a JSON `SignalPayload` — a message plus a scripted sequence of
canned model responses.

| Scenario | Demonstrates |
|---|---|
| `happy-path.json` | Two parallel tool calls in one reasoning step, then a natural stop (no tool calls). |
| `subagent-spawn.json` | A tool call with `is_subagent: true` spawns a recursive child Turn Workflow (`{turn_id}:sub:1`), which runs its own mini-loop and returns a result folded into the parent's context. |
| `interrupt-initial.json` + `interrupt-followup.json` | Run `interrupt-initial.json`, then within ~1–2s run `interrupt-followup.json` against the **same** `-session`. The in-flight tool call is cooperatively cancelled (real heartbeat-driven cancellation, not simulated), and the follow-up message is folded in at the next loop boundary. |
| `max-iterations.json` | 25 scripted responses that each request another tool call — the loop halts exactly at `max_iterations = 20` (`docs/components/temporal-workflow.md`), not before or after. |
| `subagent-merge-happy.json` | Subagent writes `result.txt` in its own subtree; parent then invokes `merge_subagent_output` and merges it into the parent's directory. Manifest (`docs/components/session-filesystem.md`, "Resolved: Subagent Merge-Back Mechanics") is written into the subagent's `tool_calls.result` by the `SubagentManifest` activity that fires post-drain. Use `-session merge-happy`. |
| `subagent-merge-conflict.json` | Subagent writes `shared.txt`, then the parent writes to the same file *after* the subagent completes, then the parent invokes `merge_subagent_output`. The destination's mtime is newer than the subagent's `turns.started_at`, so the merge skips the file and reports it in `skipped_conflicts` rather than silently overwriting the parent's version. Use `-session merge-conflict`. |
| `subagent-merge-cancelled-initial.json` + `subagent-merge-cancelled-followup.json` | Cancelled-subagent manifest path. Run `subagent-merge-cancelled-initial.json` (subagent starts a slow `shell_exec` that writes `early.txt`, sleeps 30s, then would write `late.txt`), then within ~1–2s run `subagent-merge-cancelled-followup.json` against the **same** `-session merge-cancelled`. The in-flight subagent's `shell_exec` is cooperatively cancelled, the manifest activity still fires against the cancelled subagent (so `early.txt` is visible), and the parent's next `ModelCall` merges what actually landed. |
| `claim-check-large-output.json` | Claim-check large-payload routing (`docs/components/session-filesystem.md`, "Resolved: This PV Serves as the Claim-Check Store for Large Content"). First `shell_exec` produces ~6KB of stdout — above the inline threshold, so the tool result is a reference (`claim_check_path`, `head`, `tail`, `size_bytes`, `exploration_summary`) rather than the flat-truncated head-only blob it would have been before. The next `shell_exec` reads the persisted file via plain `tail`/`wc`, proving the round trip. |
| `exploration-summary-json.json` | Type-aware Exploration Summary, JSON branch (`docs/components/context-slot.md`, "Resolved: Duties and Strategies" #2). Large JSON blob → `exploration_summary.type = "json"`, `shape.kind = "object"`, keys/value-types described deterministically (no LLM call). Second `shell_exec` reads a specific record from the claim-check path to prove random access. |
| `exploration-summary-csv.json` | Same, CSV branch. Large CSV (500 rows × 4 cols) → `exploration_summary.type = "csv"`, columns + row_count + sample rows described deterministically via `csv.Sniffer` (no LLM call). Second `shell_exec` greps a specific value against the claim-check path. |
| `exploration-summary-text.json` | Same, unstructured-text branch. Large log-like text blob → `exploration_summary.type = "text"` with `line_count`/`word_count`, plus a natural-language `summary` field when a real model is configured (degrades to counts-only when running against local fixtures/no model). Second `shell_exec` greps for ERROR lines against the claim-check path. |
| `anthropic-basic.json` | Placeholder / smoke scenario for the Anthropic provider path. Runs entirely from the fixture short-circuit today (doesn't actually hit any provider); the scenario's own message explains how to re-run against a real Anthropic-configured tier. See "Configuring real LLM providers" below. |

Example (interrupt scenario):

```sh
go run ./cmd/starter -session interrupt-demo -scenario scenarios/interrupt-initial.json
# wait ~1-2 seconds
go run ./cmd/starter -session interrupt-demo -scenario scenarios/interrupt-followup.json
```

Watch the Go worker's log and http://localhost:8233 for the running workflow —
you'll see `RequestCancelActivity` followed by the tool call resolving with a
`status: "cancelled"` observation, then the follow-up message being folded in.

## What's real vs. stubbed

**Real, matching the resolved design:**
- Workflow ID scheme (`{session_key}:turn:{n}`, `{turn_id}:sub:{n}`, `{turn_id}:act:{n}`).
- Session Coordinator ↔ Turn Workflow split, `SignalWithStart` routing, `ParentClosePolicy` (`ABANDON` for coordinator→turn, `REQUEST_CANCEL` for turn→subagent).
- Determinism-respecting loop structure: all non-deterministic work (model calls, tool calls) as Activities; loop control flow as a pure function of recorded state.
- Combined stop condition (no tool calls / `max_iterations` / `max_retries` / token budget).
- Sequential signal coalescing — a follow-up message is dequeued and folded in one at a time at loop boundaries, never batched.
- Real cooperative cancellation via activity heartbeating — not simulated. See the note on `HeartbeatTimeout` tuning below.
- Subagent recursion — same workflow type at every depth.

**Deliberately stubbed, per the implementation plan:**
- `ModelCall` returns scripted responses, not real LLM output.
- `ToolCall`, `Persist`, `Deliver`, `CompressContext` are no-ops that log what they would have done — this is unchanged by `deploy/`: Postgres/the session PV are now deployable (see below), but the stub activities don't yet actually read/write them by reference. The content contract they'll follow is resolved in `docs/components/temporal-workflow.md`; the code doesn't implement it yet.
- Subagent file merge-back (`docs/components/session-filesystem.md`) is not implemented.
- `GetVersion`/patching is not used yet — there's no prior deployed version to be compatible with (see `docs/components/temporal-workflow.md`'s versioning convention for when this starts applying).
- Real Tier B/C heartbeat interval tuning is deliberately deferred in the design; the values used here (`HeartbeatTimeout: 1s`, tool stub delay ~4s) are sized for this local demo, not derived from real tool latency data.

## Configuring real LLM providers (`docs/components/model-registry.md`)

Every language tier owns its own provider identity — a per-tier quadruple
of `PROVIDER`, `MODEL`, `API_KEY`, `BASE_URL` (BASE_URL is optional for
Anthropic). No cross-tier fallback, no shared defaults. Two supported
provider ABIs today:

- **`openai`** — every OpenAI-API-compatible endpoint. Same request/
  response shape, same SDK, differing only in base URL + model catalog.
  Covers real OpenAI, DeepSeek, Qwen/DashScope, Groq, OpenRouter,
  Crusoe, and most self-hosted inference servers.
- **`anthropic`** — Anthropic's native Messages API (separate SDK,
  different request/response shape, handled by
  `activities/activities/providers/anthropic_provider.py`).

Example env vars for the medium tier, one provider per column:

| Var | OpenAI | DeepSeek | Qwen (DashScope) | Anthropic |
|---|---|---|---|---|
| `LANGUAGE_MEDIUM_PROVIDER` | `openai` | `openai` | `openai` | `anthropic` |
| `LANGUAGE_MEDIUM_MODEL` | `gpt-4o` | `deepseek-chat` | `qwen-max` | `claude-3-5-sonnet-20241022` |
| `LANGUAGE_MEDIUM_BASE_URL` | `https://api.openai.com/v1` | `https://api.deepseek.com/v1` | `https://dashscope.aliyuncs.com/compatible-mode/v1` | *(omit — SDK default)* |
| `LANGUAGE_MEDIUM_API_KEY` | `sk-...` | `sk-...` | `sk-...` | `sk-ant-...` |

Adding a third-tier provider ABI (e.g. a non-OpenAI-compatible native
protocol not covered above) is a new class under
`activities/activities/providers/` implementing the `Provider` ABC
(`providers/base.py`) plus a dispatch case in `llm_client.get_provider`.
No other file needs to know the new provider exists.

## Deployment (`deploy/`)

`deploy/docker/` has Dockerfiles for both worker processes. `deploy/helm/`
has two Helm charts, split per `docs/components/multi-tenancy.md`'s compute
isolation decision:

- **`agent-harness-shared`** — the tenant-agnostic loop-worker pool
  (Session Coordinator + Turn Workflow). Deployed **once for the whole
  cluster**, not per tenant. `temporal.namespaces` lists every tenant
  namespace this pool serves; a namespace failing to start (unreachable, not
  yet created) is retried with backoff and doesn't affect the others.
- **`agent-harness-tenant`** — the per-tenant components: the tenant-worker
  fleet (holds that tenant's credentials, does real content I/O — ModelCall,
  ToolCall, and the message/turn bookkeeping activities all together, one
  queue), a self-hosted PV-backed Postgres instance, and the tenant PV. One
  `helm install` **per tenant**, each with its own Temporal namespace.

Both deploy against an existing Temporal server/cluster, not one either
chart manages itself. See each chart's `values.yaml` top comment and
`templates/NOTES.txt` for the full picture, including the
`ReadWriteMany`-storage-class requirement on `agent-harness-tenant` if
`tenantWorker.replicaCount > 1`.

**Validated:** `helm lint`/`helm template` against several value
combinations, `docker build`, and a real `helm install` of both charts
against a real Kubernetes cluster (namespace `agents`) — images pulled
successfully from the published registry, both workers came up healthy, and
a real turn ran end to end with correct Postgres state confirmed via direct
query, not just "pods are Running."

`agent-harness-tenant`'s own Postgres schema
(`activities/migrations/001_initial_schema.sql`) is applied automatically by
a post-install/pre-upgrade Job (`templates/postgres-migrate-hook.yaml`,
idempotent via an explicit `turns`-table existence check) — no more manual
`psql` step against a fresh tenant Postgres instance. The SQL file is
duplicated into the chart's own `files/` directory since Helm charts can't
reference files outside themselves; keep both in sync if a `002_*.sql` is
ever added.

## A note on heartbeat timing (if you tune `tool_call.py` or the `HeartbeatTimeout`)

Temporal's SDK core throttles the actual network heartbeat to roughly 80% of
`HeartbeatTimeout` (capped separately) — calling `activity.heartbeat()` more
often than that doesn't make the real RPC fire more often. If you lengthen the
stub `ToolCall`'s artificial delay in `activities/activities/tool_call.py` or
change `HeartbeatTimeout` in `workflows/internal/workflow/turn.go`, keep
`HeartbeatTimeout` meaningfully shorter than the delay, or a mid-turn interrupt
sent while the tool call is running may not have a real heartbeat land in time
to deliver the cancellation before the stub call finishes on its own.
