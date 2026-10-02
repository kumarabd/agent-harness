# Agent Harness

**A durable execution harness for a general-purpose conversational agent.**

Agent Harness runs a model-steered reason–act–observe loop — one ordinary conversational turn per message, with tools, memory, subagents, and approvals as primitives the model calls on its own judgment. It uses Temporal workflows to durably run, govern, interrupt, and recover that loop.

The result is an orchestration layer for an agent that must operate reliably over real work: coding changes, structured research, business operations, and other tasks where interruption handling, approval boundaries, and recovery from failure are part of the product — without a separate specialization layer sitting in front of the model.

## The idea

- There is no classifier, router, or per-domain control workflow deciding how a request is handled — the model reads the request and asks for what it needs, turn by turn.
- Temporal durably executes that loop: tool calls, approval waits, subagent delegation, and interruption are workflow-level events, not fragile in-process state.
- The model contributes judgment inside each step; the harness supplies primitives and a thin set of deterministic rails (approval gating, budget ceilings, compaction) around it.

Read the implemented contract in [the turn pipeline](docs/components/turn-pipeline.md).

## What the harness provides

- **Durable execution with Temporal.** Work resumes after worker failure without blindly replaying side effects.
- **One durable turn workflow.** A single `TurnWorkflow` type handles every message — top-level or subagent — governed by the model's own tool calls, not a per-domain dispatch layer.
- **Shared activities, not copied plumbing.** Model calls, tools, persistence, delivery, approvals, and delegated CLIs are reusable side-effecting units the turn workflow composes.
- **Recursive subagents.** A workflow can delegate independently scoped work to child workflows, with isolation and explicit merge-back.
- **Interruption and human interaction.** New messages, cancellation, and approvals are workflow-level events rather than fragile in-process flags.
- **Multi-tenant isolation.** Tenant boundaries extend through Temporal namespaces, dedicated worker fleets, Postgres, and session storage.
- **State, workspace, and context architecture.** Durable state lives in Postgres; session workspaces hold files and large payloads; context and memory are assembled deliberately rather than treated as an opaque prompt.

## Architecture at a glance

```text
Gateway → Session Coordinator → TurnWorkflow
                                  ├─ shared activities
                                  ├─ child workflows (subagents)
                                  ├─ Postgres state + context
                                  └─ isolated session workspace
```

The Session Coordinator serializes a session durably. A short-lived `TurnWorkflow` does the work, started fresh per message. Temporal supplies the recovery, ordering, retry, and signal semantics that make the process dependable across workers.

## Repository layout

Each deployable process is its own top-level directory and its own Go module (tied together for local builds by the root `go.work`), with `shared/` holding the handful of packages more than one service genuinely needs — everything else lives with the one service that owns it.

| Directory | Process | Owns |
|---|---|---|
| `loop-worker/` | loop-worker | Session Coordinator + Turn Workflow — the durable core loop. |
| `gateway/` | gateway | Web/Discord inbound-outbound path (text + voice). |
| `tenant-worker/` | tenant-worker (Python) | The activities Temporal dispatches by name: model calls, tools, memory, persistence. |
| `automation/` | automation | Self-serve tenant onboarding (provisions a tenant's own Helm release). |
| `router/` | router | Identity-routing front door for the first-party web client. |
| `shared/` | — | Go packages genuinely shared across services: wire types, the ID scheme, tenant-slug derivation, Clerk auth. |
| `vad-sidecar/` | vad-sidecar (Python) | Voice-activity-detection sidecar the gateway calls over gRPC. |

## Start reading

- [Overall topology](docs/01-architecture-overall-topology.md)
- [Temporal execution design](docs/02-architecture-temporal-execution.md)
- [Turn pipeline](docs/components/turn-pipeline.md)
- [Orchestrator vision](docs/04-architecture-orchestrator-vision.md)
- [Component designs](docs/components/)

## Current implementation slice

The repository includes a working Temporal proof of the session coordinator and turn loop, with Go workflows and Python activities. It uses scripted scenarios to exercise stop conditions, recursive subagents, cooperative interruption, workspace merge-back, and large-payload handling. See [`loop-worker/scenarios/`](loop-worker/scenarios/) for the runnable examples and their assertions.
