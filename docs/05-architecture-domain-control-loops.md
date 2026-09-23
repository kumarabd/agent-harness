# Agent Harness — Distributed, Scalable Architecture
## Part 5: Domain-Specific Control Loops — Process as an Executable Definition

This paper records the next architectural emphasis for the harness: author multiple, purpose-built Temporal workflows for distinct use cases and business domains. Each workflow is a durable control loop that defines how a class of work proceeds, rather than leaving that procedure solely to a model prompt or a skill document.

**This is also the harness's entire notion of a "skill."** There is no separate prose-procedure mechanism alongside it. Two earlier ideas are superseded outright, not merely extended: the harness-owned procedural-memory subsystem (`load_skill`/`RecordSkill`/`SkillDiscover`/`skill_procedures`, which auto-recorded prose procedures from transcripts and was removed in the turn-pipeline redesign — `docs/components/turn-pipeline.md`) and the never-built plan to source curated prose skills from mcp-hub's `search_skills`/`get_skill` (`docs/components/tool-registry.md`). Both are gone. A skill's body is a Temporal workflow, not a document, authored like code, discovered locally, never recorded from a transcript.

This extends, rather than replaces, the shared Session Coordinator, Turn Workflow, activity, state, workspace, and tenancy designs in Parts 1–4. It does **not** reintroduce a pre-turn classifier or router — `turn-pipeline.md`'s core principle (no classifier deciding a lane, no router deciding which subsystems to consult) stays intact. Workflow/skill selection here is a model-decided, mid-loop tool call, symmetric to how the model already decides when to call `discover_tools` or `spawn_subagent` — never a step the harness runs before the model sees the request. See *Discovery and Invocation* below.

---

### The Decision

**Make domain-specific control workflows the first-class specialization mechanism.** A workflow encodes the meaningful states, transitions, waits, approval points, retry boundaries, and completion rules for a particular kind of work. Temporal executes that definition durably.

There is not one universal loop with a growing prompt full of exceptions. There can be many carefully authored workflows, for example:

- a change-control workflow that gathers repository context, plans, requests approval where needed, delegates an implementation, verifies the result, and prepares a commit;
- a research workflow that establishes a question and evidence plan, runs bounded source collection, evaluates coverage, resolves contradictions, and produces a traceable synthesis;
- an operations workflow that classifies an incident, gathers diagnostics, applies only permitted mitigations, waits for signals or human input, and records the outcome.

These are examples of shapes, not a fixed product taxonomy. A new workflow is justified when a domain has durable process semantics worth owning explicitly: ordering, safety boundaries, external waits, failure handling, governance, or completion criteria that should not depend on a model remembering instructions.

---

### A Skill Is a Workflow, Not a Document

A conventional skill is a probabilistic instruction-following aid: a document tells a model how to approach a task, which tools to prefer, and what good output looks like. The generic agent loop still asks the model to choose whether, when, and in what order to follow those instructions — and, in this harness's own prior history, to record new ones from its own transcripts, which fragmented into partial, half-learned procedures across multi-message tasks (`turn-pipeline.md`'s "Skill recording" section has the full account).

This design replaces that document-and-recording model with an executable one. A skill *is* a domain-specific control workflow:

| Concern | Prose skill (removed) | Skill as a domain workflow |
|---|---|---|
| Process owner | Model interpretation | Executable workflow definition |
| Sequence and transitions | Suggested by instructions | Encoded and enforced in workflow state |
| Interrupts, waits, and approvals | Prompt-dependent or ad hoc | First-class Temporal signals and durable waits |
| Recovery after a worker failure | Model must reconstruct intent | Workflow resumes from recorded history |
| Auditability | What the model chose to do | State transitions and activities are explicit |
| How it's learned | Recorded from transcripts (removed — fragmented, noisy) | Authored once in code, versioned like any other artifact |
| Reuse | Reuse instructions | Reuse activities while varying process composition |

This is not merely "deterministic workflows versus probabilistic models." Model judgment remains useful—and often necessary—within a step: interpret evidence, choose among allowed actions, summarize a result, or decide *whether* a given skill applies at all. The distinction is that once a skill workflow is entered, it owns the enclosing process and the model does not get to silently skip or reorder its governing stages.

There is no fallback prose-skill tier for cases that don't warrant a full workflow. If a procedure is too lightweight to justify authoring a workflow, it stays as ordinary static-core guidance or a scratchpad note — it is not packaged as a "skill" in this harness at all.

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

### Discovery and Invocation

There is no pre-turn selector. The gateway stays a thin ingress layer (identify tenant/session, normalize the event, `SignalWithStart` to the Session Coordinator); the coordinator still always forwards to the active turn or starts an ordinary `TurnWorkflow` — exactly the flow `turn-pipeline.md` describes, unchanged. A domain workflow only enters the picture *inside* that reason-act loop, as the model's own decision, mid-turn.

The mechanism mirrors tool discovery deliberately, not by analogy:

- **Authoring** — a domain workflow is a registered Go workflow type that declares its own `{name, description, input_schema}` alongside its definition. Nothing is hand-synced to a separate manifest.
- **Discovery** — `skill-hub`, a local in-process semantic index (the same hybrid vector+FTS mechanism `shell-hub` already uses for local tool discovery, `docs/components/tool-registry.md`), built at worker startup by scanning registered workflow types. Not mcp-hub-mediated, not shared across tenants, not manually curated.
- **The meta-tool** — `discover_skills(query)` searches that index and mints matches as directly callable actions for the rest of the turn, exactly like `discover_tools`.
- **Invocation** — the model calls a minted skill by name with arguments matching its `input_schema`, on the same `tool_calls` channel as any other tool. No separate "load" or "start" step. Whichever loop makes the call — the root turn or a subagent's own turn — dispatches it as a child workflow of that registered type and blocks on it the same way it blocks on any other in-flight call; the result folds back as an observation into that same loop.

Because entry is a model-decided tool call rather than a harness-side classification step, the completeness risk is the same one already accepted for memory retrieval in `turn-pipeline.md`: a turn that should reach for a skill but doesn't call `discover_skills` simply won't. A well-written static-core hint and an accurate `description` on each skill are the mitigation, not a structural guarantee — consistent with how this harness already treats `recall` and `discover_tools`.

Once entered, the chosen workflow owns its own lifecycle:

1. establish its scoped objective from exactly the arguments it was called with — no implicit context clone (see *Skill Workflows Are Independent of Subagents* below);
2. progress through its domain stages using shared activities and, where useful, further child workflows;
3. respond to Temporal signals for interruption, cancellation, and human input;
4. persist observable outcomes and deliver a final result; and
5. complete with a domain-meaningful terminal state, returning that result to the loop that invoked it.

Long-lived coordination and short-lived execution keep the existing split: the Session Coordinator is long-lived and deliberately small, a turn or domain/skill workflow is bounded and disposable. A workflow that expects a long external wait should model that wait explicitly rather than holding a worker or hiding it inside an activity. Workflow evolution must follow Temporal-compatible versioning practices when in-flight executions may exist.

---

### Skill Workflows Are Independent of Subagents

A skill workflow and `spawn_subagent` are two separate primitives that happen to both be Temporal child workflows — not one generalizing the other, and not competing for the same use case.

- `spawn_subagent` is the model's choice to delegate open-ended scoped *reasoning* work. The child is another `TurnWorkflow` (same type, recursively), receiving a clone of the parent's session context plus an explicit brief — it decides its own approach.
- A skill workflow is a fixed *process* fragment. The child is a different, purpose-built workflow type with no context clone and no brief — only whatever its own `input_schema` declares. It is reachable identically from the root turn or from inside a subagent's own turn; nothing about invoking it depends on decomposition.

A domain workflow can itself spawn subagents or invoke further skills internally if its author designs it that way — the two primitives compose, they just don't substitute for each other.

Control workflows (of either kind) do not carry the entire system in their history. Postgres remains the durable state and audit layer. The session filesystem remains the workspace and claim-check store for files and large payloads. Context and memory are assembled through their dedicated contracts. This division lets workflow code make deterministic control decisions from IDs, status, and small structural metadata while activities handle non-deterministic I/O and large content.

Tenancy is not an afterthought to workflow selection. A selected definition must run within the tenant’s Temporal namespace and worker boundary, using that tenant’s database, credentials, permissions, and session storage. A control workflow can therefore express domain policy without creating a path around the isolation model.

---

### Authoring Criteria and Tradeoffs

Not every task deserves a new workflow. A generic turn remains appropriate for open-ended assistance or low-consequence exploration. Introduce a purpose-built workflow when the same process recurs and the harness should enforce something real: a required sequence, approval boundary, evidence standard, retry policy, handoff rule, or terminal condition.

The cost is intentional design and maintenance. Every workflow needs clear states, activity contracts, observability, versioning, and tests. That cost is worth paying only when executable process ownership creates more value than a prompt convention. Shared activities and common state/workspace contracts keep that cost from becoming duplicated infrastructure.

---

### Consequence

Agent Harness is not defined by a library of prose skills wrapped around a generic agent loop. It is defined by durable, inspectable, domain-aware control workflows that orchestrate shared capabilities and model judgment — and those workflows *are* this harness's skills, discovered locally and invoked by model decision, not a separate packaging layer bolted on top of execution.

### Notes Log
- 2026-09-21: Introduced the domain-specific control-loop strategy. The central distinction is process ownership, not a simplistic deterministic-versus-probabilistic split: models make bounded judgments inside a workflow, while the workflow encodes and enforces the process itself. Multiple purpose-built workflows share activities and infrastructure instead of duplicating generic loop plumbing.
- 2026-09-22: Reconciled against `turn-pipeline.md`'s deployed "no classifier, no router" principle, which the original "Workflow Selection" section (inbound-intent/classification-based selection) contradicted without acknowledging. Resolved: workflow/skill selection is a model-decided `discover_skills` → call-by-name tool interaction, mirroring `discover_tools`, never a pre-turn harness decision. Also resolved: this doc *is* the redesign of "skills" flagged as deferred in `turn-pipeline.md` and as never-built in `tool-registry.md` (mcp-hub's `search_skills`/`get_skill`) — both prior skill mechanisms (auto-recorded prose, and the unbuilt curated-prose plan) are superseded, not extended. Skill workflows are independent of `spawn_subagent` — no context clone, no brief, own `input_schema`-defined contract, reachable from root or subagent alike — and share the same cooperative-cancellation interrupt treatment as everything else in `turn-pipeline.md`'s interrupt table.
