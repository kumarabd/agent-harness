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

The mechanism mirrors `spawn_subagent`/`shell_exec` deliberately, not `discover_tools`: a skill is a **static, always-on capability**, present in the model's own tool list on turn 1, not something that only becomes callable after an explicit discovery step.

- **Authoring** — a domain workflow is a registered Go workflow type; its `{name, description, input_schema}` is declared once, by hand, in a small Python registry (`activities/activities/skills.py`) that both `capabilities.py`'s static `Capability` list and `llm.py`'s `TOOLS_SCHEMA` are generated from. Nothing is hand-synced to two places.
- **Discovery** — `skill-hub`, a local in-process semantic index (the same hybrid vector+FTS mechanism `shell-hub` already uses for local tool discovery, `docs/components/tool-registry.md`), built at worker startup from that same registry. `discover_skills(query)` searches it. This is an *optional* detail/search convenience for when the model wants to look something up (e.g. to check a skill's exact `input_schema`) — not how a skill becomes callable. It mints nothing and persists nothing.
- **Invocation** — the model calls a skill by its own real name with arguments matching its `input_schema`, on the same `tool_calls` channel as any other tool, from the very first step of the turn. No separate "load," "discover," or "start" step. Whichever loop makes the call — the root turn or a subagent's own turn — dispatches it as a child workflow of that registered type and blocks on it the same way it blocks on any other in-flight call; the result folds back as an observation into that same loop.

Because a skill is always in the model's own tool list, there is no completeness risk of the kind `discover_tools`/`recall` still carry (a turn that never thinks to search doesn't get the capability). An accurate `description` on each skill still matters — it's how the model judges *when* to reach for it — but reachability itself is not conditional on the model calling anything first.

Once entered, the chosen workflow owns its own lifecycle:

1. establish its scoped objective from exactly the arguments it was called with — no implicit context clone (see *Skill Workflows Are Independent of Subagents* below);
2. progress through its domain stages using shared activities and, where useful, further child workflows;
3. respond to Temporal signals for interruption, cancellation, and human input;
4. persist observable outcomes and deliver a final result; and
5. complete with a domain-meaningful terminal state, returning that result to the loop that invoked it.

Long-lived coordination and short-lived execution keep the existing split: the Session Coordinator is long-lived and deliberately small, a turn or domain/skill workflow is bounded and disposable. A workflow that expects a long external wait should model that wait explicitly rather than holding a worker or hiding it inside an activity. Workflow evolution must follow Temporal-compatible versioning practices when in-flight executions may exist.

---

### Determinism Where It Counts, Not All the Way Down

A skill's *process* — its stages, stopping conditions, approval gates — stays deterministic and harness-owned. But interpreting messy external reality (does this search result actually contain the thing being looked for, did a write actually succeed) is a model's job, not hand-parsed guesswork bolted onto a skill's own Go code — a real mistake made and corrected while building the first non-trivial skill (`journaling`, committing an entry to a Notion database whose exact API response shapes weren't known in advance).

The fix generalizes: **`turn.go`'s reason-act loop is exported as `workflow.RunReasonActLoop`**, a plain Go function (not a child workflow — it runs in-process inside whichever workflow execution calls it) factored out of `TurnWorkflow` itself. It is the *same* mechanism, not a parallel one: the same `ModelCall` dispatch, the same status/`tool_calls` stop condition, the same real `RequiresApproval` → `UserInputRequestWorkflow` path (so `ask_user` called from inside a nested reasoning turn already durably delivers to and waits on the user's real connection — no new plumbing), the same `drainResult`. `TurnWorkflow` is now a thin wrapper around it: turn-start setup, call `RunReasonActLoop`, top-level-only egress (delivery, the progress watchdog).

`skills.RunReasoningTurn(ctx, input, idSuffix, objective)` is every skill's entry point to this: it mints a scoped `turns` row (`parent_type = "skill"`, a third value alongside `"session"`/`"turn"` — its content is an objective the skill's own code authored, which fits neither existing case: not a real user message, and not derivable from a `tool_calls.arguments` row the way a subagent's kickoff is, since nothing minted one), runs `RunReasonActLoop` against it, and returns a short `{status, summary}` distilled by a Python activity (`SummarizeReasoningTurn`) — never raw message content, same reference-passing discipline as everything else crossing this boundary.

A skill's own Go code should stay minimal: read its real arguments, hand a clear objective (and, implicitly, the default tool catalog — see below) to `RunReasoningTurn`, close out with whatever comes back. The "confirm before doing something consequential" and "figure out which of several plausible matches is right" steps belong in the objective's own instructions, not as Go-coded gates — the model already has `ask_user` and already knows how to use it.

**Open, deliberately not resolved by adding a new mechanism:** a scoped reasoning turn currently gets the same default tool catalog any ordinary turn gets (no allow-list). It could technically call `spawn_subagent` or `shell_exec` even though a well-written objective would never lead there. Building a tool-restriction mechanism for this preemptively would repeat the same premature-complexity mistake `journaling`'s first draft already made once — revisit only if a real skill's behavior shows it's actually needed.

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
- 2026-09-23: Built the first two real skills (`draft_note`, a harness-validation skill exercising the mechanism end to end with no model calls of its own; `journaling`, committing an entry to a Notion database). `journaling`'s first draft hand-coded deterministic Go branching plus Python code that guessed at Notion API response shapes — corrected: skills can run their own scoped reasoning turn via the newly-exported `workflow.RunReasonActLoop` (`turn.go`'s own reason-act loop, factored out so `TurnWorkflow` and a skill's `skills.RunReasoningTurn` call the *same* mechanism, not parallel implementations), delegating interpretation of messy external data to a real model call instead of hand-parsed guesswork. New `turns.parent_type` value `"skill"` (migration 038) for the resulting scoped turn's own row. See "Determinism Where It Counts, Not All the Way Down" above for the resolved design. Also moved every skill workflow into its own Go package, `workflows/internal/workflow/skills/`, importing `workflow` only for what it already exports (`UserInputRequestWorkflow`, `RunReasonActLoop`) — `workflow` has zero dependency back, making the "skills are independent of, and unknown to, the core turn loop" claim structural rather than just documented.
- 2026-09-23: Real production failure exposed the gap the 09-22 entry's design left open: a user asked "are you able to start journaling?" and the model answered no, with zero tool calls — including no `discover_skills` — confirmed via direct Postgres inspection of the live turn's row. The on-demand-discovery design was completeness-optimistic, not structurally sound: nothing in the model's own tool list ever hinted a skill existed. A `run_skill(name, arguments)` generic dispatcher was considered and rejected as the fix — it was modeled on a misreading of `call_tool`, which is never actually model-visible (`capabilities.py`'s `call_tool` entry has `turn_kinds=frozenset()`, permanently excluded from `schema_for()`'s output; a resolved mcp-hub tool is called by the model directly under its own real name, and `call_tool` is purely `tool_call.py`'s internal proxy once it sees `resolved_server` set on a row). Resolved instead: mint each skill under its own real name **statically** — an always-on `Capability` (`capabilities.py`) with a schema generated into `llm.TOOLS_SCHEMA` from a small hand-maintained registry (`activities/activities/skills.py`) — exactly the same shape `spawn_subagent`/`shell_exec` already have, present from turn 1, no discovery step required. `discover_skills` is demoted to an optional search/detail convenience; it mints and persists nothing. Also collapsed `ToolCallRef`'s `IsSkill bool` + `ResolvedWorkflowType string` into one field, `UseSkill string` (Go) / `use_skill: str` (Python) — empty means not a skill, non-empty is the workflow type to dispatch — mirroring how `resolved_server`/`resolved_tool` already need no companion "is this resolved" boolean. `turn.go`'s dispatch branch is now `runSkill(...)`, gated on `tc.UseSkill != ""`. See "Discovery and Invocation" above for the resulting design.
- 2026-09-24: Deployed the 09-23 fix, then hit the *same user-visible symptom again* on the very next redeploy — verified this was not a stale/skipped deploy (migration 039 applied fresh; the running worker pod's own `capabilities.BY_NAME["journaling"]` and `schema_for(REASONING)` both confirmed present and correct). Root cause was a completely different gap, not a regression of the 09-23 fix: the failing turn had `messages.mode = "voice"` (a spoken request from the mobile app), which makes `model_call.py` swap in `llm.VOICE_SYSTEM_PROMPT` instead of `DEFAULT_SYSTEM_PROMPT` — a standalone prompt (mirrored by `prompts.go`'s `voiceSystemPromptText` for Discord voice) that only ever covered TTS-safe formatting and the `report_status` contract, never `DEFAULT_SYSTEM_PROMPT`'s "PROVISIONING" section telling the model tools/skills exist and are already in its own tool list. `journaling` was genuinely present in that turn's function-calling schema (tool *availability* doesn't depend on which system prompt is active — confirmed live), but the model made zero tool calls and told the user outright it couldn't save anything, because nothing in its framing ever suggested checking. Fixed by adding a spoken-register equivalent of the provisioning bullet to both `VOICE_SYSTEM_PROMPT` and `voiceSystemPromptText`, kept in step per their existing hand-sync convention. Lesson: a capability being schema-visible is necessary but not sufficient — every system-prompt variant needs its own "you have tools, use them" framing, not just the default one.
