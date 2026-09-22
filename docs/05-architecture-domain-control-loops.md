# Agent Harness — Distributed, Scalable Architecture
## Part 5: Domain-Specific Control Loops — Process as an Executable Definition

This paper records the next architectural emphasis for the harness: author multiple, purpose-built Temporal workflows for distinct use cases and business domains. Each workflow is a durable control loop that defines how a class of work proceeds, rather than leaving that procedure solely to a model prompt or a skill document.

This extends, rather than replaces, the shared Session Coordinator, Turn Workflow, activity, state, workspace, and tenancy designs in Parts 1–4.

---

### The Decision

**Make domain-specific control workflows the first-class specialization mechanism.** A workflow encodes the meaningful states, transitions, waits, approval points, retry boundaries, and completion rules for a particular kind of work. Temporal executes that definition durably.

There is not one universal loop with a growing prompt full of exceptions. There can be many carefully authored workflows, for example:

- a change-control workflow that gathers repository context, plans, requests approval where needed, delegates an implementation, verifies the result, and prepares a commit;
- a research workflow that establishes a question and evidence plan, runs bounded source collection, evaluates coverage, resolves contradictions, and produces a traceable synthesis;
- an operations workflow that classifies an incident, gathers diagnostics, applies only permitted mitigations, waits for signals or human input, and records the outcome.

These are examples of shapes, not a fixed product taxonomy. A new workflow is justified when a domain has durable process semantics worth owning explicitly: ordering, safety boundaries, external waits, failure handling, governance, or completion criteria that should not depend on a model remembering instructions.

---

### Why a Workflow Is Not a Skill

A conventional skill is usually a probabilistic instruction-following aid: a document tells a model how to approach a task, which tools to prefer, and what good output looks like. The generic agent loop still asks the model to choose whether, when, and in what order to follow those instructions.

A domain control workflow changes where authority lives:

| Concern | Skill inside a generic loop | Domain-specific control workflow |
|---|---|---|
| Process owner | Model interpretation | Executable workflow definition |
| Sequence and transitions | Suggested by instructions | Encoded and enforced in workflow state |
| Interrupts, waits, and approvals | Prompt-dependent or ad hoc | First-class Temporal signals and durable waits |
| Recovery after a worker failure | Model must reconstruct intent | Workflow resumes from recorded history |
| Auditability | What the model chose to do | State transitions and activities are explicit |
| Reuse | Reuse instructions | Reuse activities while varying process composition |

This is not merely “deterministic workflows versus probabilistic models.” Model judgment remains useful—and often necessary—within a step: classify an input, propose a plan, interpret evidence, choose among allowed actions, or summarize a result. The distinction is that the workflow owns the enclosing process and the model does not get to silently skip or reorder its governing stages.

Skills can still exist as optional knowledge packaging, including for interoperability with external agent ecosystems. They are not required by this architecture. A skill that only restates workflow steps is redundant; the workflow is the executable version of that specialization.

---

### Composition: Many Workflows, Shared Activities

Multiple workflows do **not** imply copying the low-level “call a model, invoke a tool, persist state” loop into every domain implementation. Activities remain the shared, side-effecting building blocks:

- model calls and context assembly;
- tool and delegated-agent execution;
- state persistence, retrieval, and memory access;
- outbound delivery, approval requests, and other user interaction;
- workspace operations, claim-check storage, and explicit subagent merge-back.

Each workflow chooses and sequences these activities according to its domain. The activity contract remains stable, observable, retryable, and tenant-scoped; the workflow supplies the process policy. A change-control workflow and a research workflow may both call a model, search, delegate work, and persist results, but they need not share the same gates, evidence thresholds, or finish conditions.

This is the intended boundary:

```text
shared activities + shared state/workspace contracts
                         ↓
      authored Temporal workflows for specific domains
                         ↓
      durable, governed execution of a domain process
```

---

### Workflow Selection and Lifecycle

The gateway remains a thin ingress layer. It identifies tenant and session, normalizes the inbound event, and addresses the Session Coordinator through `SignalWithStart`. The coordinator remains the durable per-session control plane: it serializes active work and turns later input into an interrupt or a new unit of work as appropriate.

Workflow selection is an explicit routing decision, not an accidental prompt side effect. A selector may use the inbound intent, tenant configuration, session state, a user-selected mode, or an inexpensive classification step to choose a suitable workflow definition. That selection—and the reason or policy version behind it—should be recorded in durable state so it is reviewable and replay-safe.

The chosen workflow then owns its lifecycle:

1. establish the scoped objective and load only the context it needs;
2. progress through its domain stages using shared activities and, where useful, child workflows;
3. respond to Temporal signals for interruption, cancellation, and human input;
4. persist observable outcomes and deliver a final result; and
5. complete with a domain-meaningful terminal state, or hand control back to the coordinator for later work.

Long-lived coordination and short-lived execution should keep the existing split: the Session Coordinator is long-lived and deliberately small, while a turn or domain workflow is bounded and disposable. A workflow that expects a long external wait should model that wait explicitly rather than holding a worker or hiding it inside an activity. Workflow evolution must follow Temporal-compatible versioning practices when in-flight executions may exist.

---

### Subagents, State, and Isolation

Control workflows can recursively spawn child workflows for independently scoped work. That preserves the existing recursive-subagent model: a child has its own bounded objective, context, activity history, and isolated workspace; its result returns through an explicit observation or merge-back contract.

The workflow does not carry the entire system in its history. Postgres remains the durable state and audit layer. The session filesystem remains the workspace and claim-check store for files and large payloads. Context and memory are assembled through their dedicated contracts. This division lets workflow code make deterministic control decisions from IDs, status, and small structural metadata while activities handle non-deterministic I/O and large content.

Tenancy is not an afterthought to workflow selection. A selected definition must run within the tenant’s Temporal namespace and worker boundary, using that tenant’s database, credentials, permissions, and session storage. A control workflow can therefore express domain policy without creating a path around the isolation model.

---

### Authoring Criteria and Tradeoffs

Not every task deserves a new workflow. A generic turn remains appropriate for open-ended assistance or low-consequence exploration. Introduce a purpose-built workflow when the same process recurs and the harness should enforce something real: a required sequence, approval boundary, evidence standard, retry policy, handoff rule, or terminal condition.

The cost is intentional design and maintenance. Every workflow needs clear states, activity contracts, observability, versioning, and tests. That cost is worth paying only when executable process ownership creates more value than a prompt convention. Shared activities and common state/workspace contracts keep that cost from becoming duplicated infrastructure.

---

### Consequence

Agent Harness is not defined by a library of skills wrapped around a generic agent loop. It is defined by durable, inspectable, domain-aware control workflows that orchestrate shared capabilities and model judgment. The workflow becomes the executable form of specialization; skills are optional helpers, not the foundation of execution.

### Notes Log
- 2026-09-21: Introduced the domain-specific control-loop strategy. The central distinction is process ownership, not a simplistic deterministic-versus-probabilistic split: models make bounded judgments inside a workflow, while the workflow encodes and enforces the process itself. Multiple purpose-built workflows share activities and infrastructure instead of duplicating generic loop plumbing. Skills remain optional guidance or interoperability packaging, not a required first-class execution mechanism.
