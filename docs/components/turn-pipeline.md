# Component: Turn Pipeline

> STATUS: **DESIGN — target architecture.** This is the single reference for how a
> turn runs. It replaces the classify / lane / routing / planning machinery with a
> model-steered reason-act loop over a thin set of deterministic rails. The
> "What this replaces" section at the end lists the components to remove.

### Role (one line)

Everything between "a user message arrives" and "the response is delivered" — one
durable `TurnWorkflow` running a reason-act loop in which **the model decides how
to solve the task**, and the harness supplies primitives (memory, skills, tools,
subagents, a scratchpad, delivery) and safety rails (approval gating, budget
ceilings, compaction).

### Core principle

The model steers; the harness supports. There is no classifier deciding a lane,
no router deciding which subsystems to consult, no planning workflow deciding the
shape of the work. The model reads the request and, turn by turn, asks for what
it needs and acts. The pattern by which a task gets solved — a direct answer, a
retrieve-then-answer, an explore-then-act, a decomposition into subagents, an
iterative build against a scratchpad — is the *emergent shape* of that loop, not
a decision the harness makes or the model announces.

The harness keeps opinions only where a guarantee is required: irreversible
actions, spend, context-window safety, and always-on recall. That is roughly
twenty lines of deterministic code guarding the blast radius; the model is free
inside it.

---

## The turn lifecycle

```
message arrives
   │
   ▼
CoordinatorWorkflow ──► forward to the active turn (signal)  ─┐
   │  no active turn                                          │
   ▼                                                          │
InsertMessage + start TurnWorkflow                            │
   │                                                          │
   ▼                                                          │
┌─ reason-act loop ──────────────────────────────────────┐    │
│  ModelCall  → assemble prompt, call model, mint tools   │    │
│     │                                                   │    │
│     ├─ status: done   → break                           │    │
│     ├─ status: blocked → park on ask_user, then resume  │    │
│     └─ status: working →                                │    │
│           tool_calls? ── fan out (activities +          │    │
│           │              subagent child workflows)      │    │
│           │              observe, append results        │◄───┘  (follow-up folds in here)
│           └── none ──── loop again (model still thinking)│
└────────────────────────────────────────────────────────┘
   │
   ▼
deliver final response → Persist → complete
```

There is no special "first turn." Turn 1 is an ordinary loop iteration: the model
either answers and sets `status: done`, or emits a first `message` plus first
actions with `status: working` and the loop continues.

### Stop conditions

The loop ends when any of these is true (whichever fires, the turn completes with
whatever it has — never a hard crash):

- `status: done` **and** no follow-up message is queued at that boundary.
- `iterations >= ceiling` — a hard cap the model cannot raise (see *Iteration
  budget*).
- token/cost budget exhausted.

`status: done` racing an incoming message: the loop checks the follow-up queue
**before** acting on `done`. A pending message means `done` was decided on stale
input — fold the message in, keep looping, let the model re-decide.

---

## Model I/O schema

The model's output has four groups. A field earns its place only if the harness
must mechanically branch on it and it is not itself an action — everything else
is a tool call or prose.

```
ModelTurnOutput {
  # ── content ──
  message: str | null            # user-facing text: a progress note, a partial
                                 # answer, the final answer, or null on a pure work step

  # ── lifecycle (the only thing the harness branches on) ──
  status: "working" | "done" | "blocked"

  # ── actions (the model's real control channel) ──
  tool_calls: [ ... ]            # ordinary tools AND meta-tools:
                                 #   search_memory · discover_tools · load_skill
                                 #   spawn_subagent · ask_user

  # ── advisory (recorded / used if present, safe to omit) ──
  next_step: {
     note: str | null            # note-to-self, injected into the next call's context
     modality, tier              # the model selects its own model for the next step
     est_remaining_steps: int    # feeds the iteration-ceiling negotiation
  }
}
```

Deliberately **not** fields: `needs_memory` / `needs_tools` / `needs_skills`
(those are meta-tool calls), `plan` (that is the scratchpad, or prose in
`message`), `confidence` (self-reported model confidence is poorly calibrated;
do not route on it), `intent` / `complexity` (computed after the fact from the
finished trace if dashboards are wanted, never on the critical path).

`message` is for **content** only — a partial answer, a finding, "here is my read
before I start," the final response. It never carries "still working" liveness;
that is the watchdog's job (see *Progress watchdog*).

---

## Prompt assembly

```
prompt = static core  +  pinned context  +  LCM-assembled conversation
tools  = always-on core set  +  whatever discover_tools has added this turn
```

**Static core** — a stable, minimal, cached prefix, identical on every call
(including the calls that are just "look at this tool result and continue"), so it
is never noise. It contains only:

- identity
- solving instincts (the pattern taxonomy as illustrative guidance, explicitly
  not an enum — default to the lightest approach that fits, escalate on friction,
  switch freely)
- tool-use norms
- **the three rules** (see *Information-seeking contract*)
- scratchpad usage
- safety framing
- the meta-tool catalog (names + when each applies)

Nothing task-type-specific beyond a light touch.

**Pinned context** — a small set re-injected verbatim every call, never
compacted: the turn's scratchpad file (if it exists), the task framing. This is
what a file-backed scratchpad buys — it lives outside LCM entirely, and assembly
tails it in every call.

**LCM-assembled conversation** — the transcript. Append-only, compacted as it
grows (verbatim window + summary DAG). Everything retrieved during the turn flows
*into* this stream as ordinary observation messages: a `search_memory` result, a
`load_skill` procedure, a tool result, a subagent result. LCM compacts them
uniformly with everything else. There is no separate managed "memory section" or
"skills section" and no per-section budget shedding — that logic collapses into
LCM's normal compaction.

**Tools param** — assembled separately because callable function schemas are a
provider-request parameter, not messages. The core set (read / write / shell /
search / list) is always present so exploration and coding tasks start without a
discovery round-trip; `discover_tools` adds exotic capabilities (a weather API, a
maps service) for the rest of the turn, read from the per-turn discovered set.

**No ambient memory digest.** An earlier draft of this design added a thin
always-on profile/memory block injected every turn. `memory-slot.md` ("Resolved:
Entity Facts as a Task-Matched Procedure — No First-Class Digest", 2026-09-05)
had already examined and rejected exactly that — no genesis population, no
staleness cache, no non-shed section. Recall instead rests on: the `search_memory`
meta-tool, the "retrieve before answering" rule, the "don't guess — ask or
`create_intention`" rule, and (over time) a learned entity-lookup procedure the
model pulls via `load_skill`. The completeness risk (a turn that needs a fact
never triggering the lookup) is consciously accepted; a well-authored procedure
is the mitigation, not a structural guarantee.

The result tailors itself: turn 1 is lean; turn 6 of a research task carries
skills, discovered tools, a scratchpad, and memory hits — the prompt grows with
the work, not up front.

---

## Meta-tools

The model's provisioning actions, in the same `tool_calls` channel as ordinary
tools, marked a `meta` category (excluded from approval gating and, optionally,
from the user-visible stream and the iteration budget):

| Tool | Effect |
|---|---|
| `search_memory(query)` | returns matched long-term memory as an observation |
| `discover_tools(query)` | matched tool schemas become callable for the rest of the turn |
| `load_skill(name)` | the procedure text is appended to context as an observation |
| `spawn_subagent(brief, …)` | starts a child `TurnWorkflow` (see *Subagents*) |
| `ask_user(question, options?)` | parks the turn on a user-input request (see *Interrupts*) |

The scratchpad uses the ordinary file tools against a session-scoped path
(`…/turn/<seq>/scratchpad.md`); assembly auto-tails that path, so there is no
dedicated scratchpad tool.

Provisioning is a round-trip by nature — the model cannot act on a tool it does
not yet have a schema for — so the static core tells the model to **batch its
provisioning** (request skills + tools + memory in one step) and to take the
first concrete step in the same response where possible.

### Information-seeking contract (the three rules)

These are rules, not guidance. Eval them explicitly.

1. **Reach for a primitive before answering from training knowledge.** If the
   request needs current, user-specific, or environment-specific facts, retrieve
   first — do not guess.
2. **Disclose every fallback.** When the model does answer from training
   knowledge, it says so, every time: *"I don't have current data on this — from
   general knowledge, …"*.
3. **Ask, don't assume.** An underspecified request, an ambiguous target, or a
   choice with real consequences → `ask_user`.

---

## Subagents

The only decomposition primitive. `spawn_subagent` starts a child `TurnWorkflow`
(same workflow type, recursively), `ParentClosePolicy: REQUEST_CANCEL`.

The child receives a **clone of the parent's session context** plus an
**explicit brief** the parent writes: what the specific job is, what to return,
what not to touch. The clone supplies knowledge; the brief supplies focus — both
are required. The parent decides what to do with the child's result (merge-back
is model-driven, read from Postgres by the parent's next `ModelCall`).

A subagent-issued `spawn_subagent` must show genuine narrowing — `delegated_scope`
and `kept_work` both non-blank — or it is rejected at mint time with an
observation telling the model to do the work directly. Root's own sibling fan-out
(several `spawn_subagent` calls in one response) is unaffected.

On subagent completion or cancellation, a `SubagentManifest` activity records its
changed-file list against its own turn id before the parent's next `ModelCall`
reads it.

---

## Progress watchdog

Keeps the user informed while the model is heads-down, without making the model
responsible for narrating. A `workflow.Go` goroutine inside `TurnWorkflow`, armed
only for **top-level turns** (`ParentType == "session"` — a subagent has no
external delivery target), scoped to the turn's lifetime, torn down automatically
when the workflow returns.

```
arm a durable timer (backoff ladder, discord: {20s, 45s, 90s})
progress since the timer was armed → collapse backoff, don't ping
timer fires with no progress       → StatusPing activity + push, then step backoff
turn's reason-act loop exits       → set turnDone, goroutine returns
```

"Progress" is a monotonic counter the turn's main goroutine bumps on every real
step (a new reason-act iteration, the final delivery). The delays are **not**
sub-10s: a routine `ModelCall` already runs tens of seconds, so a tighter ladder
would fire on every normal turn. Numeric tuning is deferred — adjust with real
latency data.

`StatusPing` (tenant-worker) reads the turn's current state — latest `tool_calls`
row (name, status), assistant-message count, any error — and returns a one-liner
built from a template (`"Working on it — {tool_name}, step {n}"` / `"…hit a snag
on {tool_name}, retrying."` / `"Still working on this…"`). It writes the line to
its **own** table, `turn_status_pings` (migration `029`) — deliberately not a
widened `turn_deliveries` (that's the streamed-chunk content ledger). The ping is
**transient**: it never enters `messages`, never enters LCM context. The workflow
then pushes it via the platform's delivery queue (`DiscordDeliverStatus` on the
connection's embedded worker).

**Discord text only today.** Voice already has its filler-audio player
(`voice_filler_player.go`) covering the same gap better; Web surfaces progress
through its poll. A third platform is a one-line addition (`statusPingBackoff` +
`statusDeliverActivity` + a gateway `DeliverStatus`).

This is distinct from Temporal's activity heartbeat (worker liveness) and from the
model's `message` (content). Three separate liveness concepts, kept separate.

A genuinely wedged workflow is a different problem: a `WorkflowRunTimeout`
(30 min, placeholder) on the `TurnWorkflow` child bounds it, and the coordinator
(which holds the child future) calls `StatusPing(turnID, "wedged")` + push on the
child's error, since `failTurn` cannot run when the workflow is killed.

---

## Interrupt model

A follow-up message arriving mid-turn is a real cancellation of whatever is in
flight, not a queue-after.

### Invariants

- The signal handler **only appends** to a follow-up queue — a pure,
  replay-deterministic operation. It never branches on arrival timing.
- Dequeue happens at explicit loop boundaries, **one message per boundary**, one
  reasoning pass each. A burst of three messages produces three passes, not one
  merged blob.
- Every cancelled action gets a `status: "cancelled"` observation appended
  alongside the new message — nothing interrupted disappears silently.
- Cancellation is cooperative and real: `WAIT_CANCELLATION_COMPLETED` on
  activities, `RequestCancelChildWorkflowExecution` cascading to subagents. Never
  `TERMINATE`, never `ABANDON`.
- Interrupt latency is bounded (next heartbeat + teardown), not instant.

### Scenarios

| In flight when the message arrives | Action | Latency |
|---|---|---|
| Between iterations | fold in at the boundary, next `ModelCall` sees it | instant |
| `ModelCall`, non-streaming | let it finish (cheap), fold in at the boundary | one model call |
| `ModelCall`, streaming (turn-1 voice/discord) | barge-in cancels it; partial output → `[response interrupted]` observation | near-instant |
| Tool calls (activities) | cancel context, await settle; non-cancellable Tier-A tools run to completion; `cancelled` observation per call | heartbeat + teardown |
| Subagent (child workflow) | cascade cancel; `SubagentManifest` still runs for partial file work; `cancelled` observation | deepest in-flight tool's heartbeat |
| Multiple queued | FIFO, one per boundary | — |
| Blocked on `ask_user` | **the message resolves the block** (below) | instant |
| Hard compaction (blocking) | let it finish — it makes the next `ModelCall` viable — then fold in | compaction completes |
| Final delivery | voice: barge-in cancels TTS; text: let it finish. Turn is terminal → the message starts a **new turn** at the coordinator | platform-dependent |
| Watchdog `StatusPing` | no interaction — independent, best-effort | — |

### `ask_user`

`ask_user` is dispatched as a `UserInputRequestWorkflow` child (`Kind:
"question"`) and joins the turn's normal tool-call fan-out — so the existing
`workflow.Await(all-settled OR a follow-up message)` already races it two ways,
never a bare `.Get()`:

- **A real answer** (button click → `UserInputResponse` signal, or web `/respond`)
  resolves the child. `RequestUserInput` reads the question + options from
  `tool_calls.arguments` (the workflow never holds them); `CloseUserInput` writes
  the answer back into the `ask_user` `tool_calls` row (`status: ok`,
  `{"answer": …}`), so the next `ModelCall` sees it as an observation.
- **A follow-up message** arriving instead pre-empts the wait: the loop cancels
  the child (its row → `cancelled`) and folds the message in as the next user
  turn. The model connects it to the question from context.

Implemented in Phase 5 — the `blocked` status branch itself lands with Phase 7
(the model doesn't author `status` yet).

---

## Deterministic rails

The small fixed set of things the harness guarantees rather than leaves to the
model:

- **Approval gating** — policy-driven (`permissions`), evaluated at tool
  execution time. The model may also choose to confirm anything via `ask_user`,
  but the irreversible-action gate is structural and not the model's decision.
- **Iteration ceiling** — the model proposes `est_remaining_steps`; the harness
  enforces a hard cap above it that the model cannot raise. At the model's own
  estimate it may request an extension with a one-line justification, granted up
  to the cap. Estimate-vs-actual is logged as a calibration signal.
- **Token / cost budget** — a per-turn ceiling, checked each iteration.
- **Compaction ceiling** — a hard, blocking compaction at the context-window
  limit, regardless of what the model wants (see *Safety mechanisms*).
- **Thrash detector** *(deferred)* — the same tool with near-identical arguments
  N times → a forced reassessment observation. Start with only the outer
  ceilings and observe real behavior first.

---

## Safety mechanisms (involuntary)

- **Compaction** — evaluated every iteration against the active model's context
  window. *Soft*: fire an async, non-blocking compaction (a cheap no-op when
  there is nothing new). *Hard*: block the turn until compaction completes, so
  the next `ModelCall` assembles a smaller context.
- **Memory write-back** — on session completion (the coordinator's idle-timeout
  exit) and on a hard compaction boundary, dispatch a detached `WriteMemory`
  child. Not per turn.
- **`WorkflowRunTimeout`** on every `TurnWorkflow`, with the coordinator
  delivering a fallback notice on child error/timeout.

---

## Skill recording

After a turn that did real multi-step work and completed cleanly, a detached
`RecordSkill` child sweeps that turn's trajectory (match → reinforce; no match +
success → generalize a new procedure). Gated on the turn having used tools across
multiple iterations.

**Known regression:** recording is now per-turn. Teaching the agent something
across several messages fragments into several partial procedures. Accepted for
v1; revisit if it proves noisy.

---

## What this replaces

Remove:

- **Workflows** — `PlanWorkflow`, `CheckpointWorkflow`, `RoutingWorkflow`.
- **Workflow code** — `Route` / `laneIsDeliberate` (`routing.go`); `dispatchWork`
  / `WorkKind` / `WorkAttach` / `PlanDone` / plan-abandon handling
  (`dispatch.go`, `coordinator.go`) — `dispatch` collapses to `InsertMessage` +
  start `TurnWorkflow`; the coordinator collapses to "forward to the active turn
  or start one."
- **Activities** — `ClassifyRequest`, `NextCheckpoint`, `MarkCheckpointDone`,
  `RenderPlan`, `ResolveOpenPlan`, `SkillDiscover` staged-under-plan logic. Plan
  meta-tool peeling (`propose_plan` / `checkpoint_done`) leaves `ModelCall`.
- **Modules** — `plan.py`, `plan_resolve.py`, the PLAN.md file store; the
  section-model + budget-shed logic in `prompt.py` (assembly shrinks to "pinned
  context + tools param"; LCM does the rest).
- **Schema / types** — `TaskRepresentation`, `RoutingPlan` / `RoutingResult`, the
  `PlanningMode` / `PlanHandling` / `PlanID` / `Task` / `OfferDeliveryTools` flag
  soup on `TurnInput`, the `NeedsApproval` field on `TurnResult`, `turn_plan` /
  plan tables.
- **Docs** — `lane-model.md`, `episode-lifecycle.md`,
  `request-pipeline.md` + `request-pipeline/`. Fold anything still live from
  `temporal-workflow.md` (the reference-passing contract, determinism
  constraints, the id scheme) into this doc or keep that doc trimmed to those.

Keep, unchanged: the reference-passing contract (the workflow holds only ids, all
content I/O is in activities), the `{turn_id}` = workflow-id / `:act:n` / `:sub:n`
id scheme, interrupts, `Persist` / `Deliver` + delivery recovery, the streaming
chunk path, `IntentionWorkflow` (proactivity — a genuinely separate concern),
`UserInputRequestWorkflow`.

---

## Open questions

- **Provisioning latency** — the common non-trivial turn now costs ≥2 model calls
  (provision → act). Acceptable for v1; measure and revisit (a larger always-on
  core set, or a fast provisioning tier).
- **Static-core ceiling** — how many behavioral rules the core can hold before
  the model starts dropping them. Far from it now; watch as capabilities grow.
- **Trace classification** — the after-the-fact job that labels a finished turn's
  shape for dashboards. Optional, build when analytics are wanted.
- **External stuck-turn watchdog** — only if in-practice workflows genuinely
  wedge; the run timeout + coordinator fallback covers the rest.
- **Numeric tuning** — every threshold here (backoff, budgets, compaction
  fractions, iteration cap) is a placeholder pending real data.
