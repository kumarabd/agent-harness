# Agent Harness

**A durable execution harness for agents whose operating procedures matter.**

Most agent systems put a general-purpose reason–act–observe loop around a model and ask it to follow a skill: a probabilistic set of instructions about what to do next. Agent Harness makes the process itself a first-class executable definition. It uses Temporal workflows to durably run, govern, interrupt, and recover that process.

The result is an orchestration layer for agents that must operate reliably over real work: coding changes, structured research, business operations, and other domain-specific processes where the order of work, approval boundaries, and completion criteria are part of the product.

## The idea

The specialization normally carried by a “skill” moves into an authored, domain-specific control workflow.

- A generic agent loop asks a model to remember and follow a procedure.
- A control workflow encodes the procedure as durable state, explicit transitions, waits, retries, and lifecycle rules.
- The model still contributes judgment inside bounded steps; the workflow owns and enforces how the work progresses.

Skills remain useful as optional guidance or compatibility packaging, but they are not a required execution layer. When a procedure is important enough to govern, audit, interrupt, or resume correctly, it belongs in a workflow.

Read the full proposal in [the domain control-loop strategy](docs/05-architecture-domain-control-loops.md).

## What the harness provides

- **Durable execution with Temporal.** Work resumes after worker failure without blindly replaying side effects.
- **Purpose-built control workflows.** Carefully authored workflows can model distinct use cases and business domains while sharing a common activity library.
- **Shared activities, not copied plumbing.** Model calls, tools, persistence, delivery, approvals, and delegated CLIs are reusable side-effecting units; workflows compose them into different processes.
- **Recursive subagents.** A workflow can delegate independently scoped work to child workflows, with isolation and explicit merge-back.
- **Interruption and human interaction.** New messages, cancellation, and approvals are workflow-level events rather than fragile in-process flags.
- **Multi-tenant isolation.** Tenant boundaries extend through Temporal namespaces, dedicated worker fleets, Postgres, and session storage.
- **State, workspace, and context architecture.** Durable state lives in Postgres; session workspaces hold files and large payloads; context and memory are assembled deliberately rather than treated as an opaque prompt.

## Architecture at a glance

```text
Gateway → Session Coordinator → purpose-built Turn / Control Workflow
                                  ├─ shared activities
                                  ├─ child workflows (subagents)
                                  ├─ Postgres state + context
                                  └─ isolated session workspace
```

The Session Coordinator serializes a session durably. Short-lived turn and domain workflows do the work. Temporal supplies the recovery, ordering, retry, and signal semantics that make the process dependable across workers.

## Start reading

- [Overall topology](docs/01-architecture-overall-topology.md)
- [Temporal execution design](docs/02-architecture-temporal-execution.md)
- [Domain-specific control-loop strategy](docs/05-architecture-domain-control-loops.md)
- [Orchestrator vision](docs/04-architecture-orchestrator-vision.md)
- [Component designs](docs/components/)

## Current implementation slice

The repository includes a working Temporal proof of the session coordinator and turn loop, with Go workflows and Python activities. It uses scripted scenarios to exercise stop conditions, recursive subagents, cooperative interruption, workspace merge-back, and large-payload handling. See [`workflows/scenarios/`](workflows/scenarios/) for the runnable examples and their assertions.
