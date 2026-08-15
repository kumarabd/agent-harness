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

# 2. Go workflow worker
cd workflows
go run ./cmd/worker

# 3. Python activity worker
cd activities
.venv/bin/python -m activities.worker

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

## Deployment (`deploy/`)

`deploy/docker/` has Dockerfiles for both worker processes. `deploy/helm/`
has two Helm charts, split per `docs/components/multi-tenancy.md`'s compute
isolation decision:

- **`agent-harness-shared`** — the tenant-agnostic workflow-worker pool
  (Session Coordinator + Turn Workflow). Deployed **once for the whole
  cluster**, not per tenant. `temporal.namespaces` lists every tenant
  namespace this pool serves; a namespace failing to start (unreachable, not
  yet created) is retried with backoff and doesn't affect the others.
- **`agent-harness-tenant`** — the per-tenant components: the activity
  worker fleet (holds that tenant's credentials, does real content I/O), a
  self-hosted PV-backed Postgres instance, and the tenant PV. One
  `helm install` **per tenant**, each with its own Temporal namespace.

Both deploy against an existing Temporal server/cluster, not one either
chart manages itself. See each chart's `values.yaml` top comment and
`templates/NOTES.txt` for the full picture, including the
`ReadWriteMany`-storage-class requirement on `agent-harness-tenant` if
`activityWorker.replicaCount > 1`.

**Validated:** `helm lint`/`helm template` against several value
combinations, `docker build`, and a real `helm install` of both charts
against a real Kubernetes cluster (namespace `agents`) — images pulled
successfully from the published registry, both workers came up healthy, and
a real turn ran end to end with correct Postgres state confirmed via direct
query, not just "pods are Running."

**Known gap:** neither chart runs the Postgres schema migration
(`activities/migrations/001_initial_schema.sql`) automatically — it has to
be applied by hand against a fresh tenant Postgres instance before its
activity worker can do anything useful. No init job/mechanism for this
exists yet.

## A note on heartbeat timing (if you tune `tool_call.py` or the `HeartbeatTimeout`)

Temporal's SDK core throttles the actual network heartbeat to roughly 80% of
`HeartbeatTimeout` (capped separately) — calling `activity.heartbeat()` more
often than that doesn't make the real RPC fire more often. If you lengthen the
stub `ToolCall`'s artificial delay in `activities/activities/tool_call.py` or
change `HeartbeatTimeout` in `workflows/internal/workflow/turn.go`, keep
`HeartbeatTimeout` meaningfully shorter than the delay, or a mid-turn interrupt
sent while the tool call is running may not have a real heartbeat land in time
to deliver the cancellation before the stub call finishes on its own.
