# Component: Proactivity — Intentions

> STATUS: DESIGN v3 (2026-09-02). Supersedes v2 ("first-class hand-built
> components"), rejected as a parallel mini-architecture.
>
> **BUILD — Phase 1 substrate, branch `proactivity-substrate` (compiles, Go
> tests green, NOT deployed):**
> - **1a** — `turns.initiated_by` / `episodes.initiated_by` (migration `020`);
>   coordinator `Wake` signal → proactive turn (`initiated_by="intn:<id>"`) or
>   fold-in to the live turn; `startSessionTurn` helper.
> - **1b-i** — `IntentionWorkflow` (loop-worker): `time`/`deadline` one-shot,
>   `condition`/`state`/`event` poll loop, `inactivity` idle timer;
>   `revise`/`snooze`/`reset` signals, `status` query; `FireIntention` activity
>   (SignalWithStart the coordinator). 5 workflow tests.
> - **1b-ii** — 6 agent tools (`create` / `list` / `inspect` / `revise` /
>   `snooze` / `cancel_intention`) → handlers in `tools_intention.py` over
>   `ctx.temporal_client` (threaded into `ToolCallActivity`). No new activities.
>   **Consolidated to 2 model-facing tools 2026-09-04** (`create_intention` +
>   `manage_intention(action, …)`) — see "The agent's tools" below; the 5
>   handlers stay as `manage_intention`'s internal implementations.
> - **1b-iii** — real `CheckCondition` (mcp-hub probe → fast-tier predicate
>   judge).
> - **recurring** — `kind=schedule` (+ `cron` UTC / `every_seconds`) → a Temporal
>   Schedule (`intn-sched:<scope>:<slug>`) starting a one-shot
>   `IntentionWorkflow{kind:time}` per tick; `list` / `inspect` / `cancel` handle
>   schedule ids; `revise` / `snooze` don't apply (cancel + recreate).
>
> **v1 deviations from this doc, deferred:**
> - Intentions key on the **user-stable scope** (`ids.UserScopeOf` — session_key
>   with any `:session:`/`:thread:` suffix stripped): `intn:<scope>:<slug>`. For
>   web that scope is the user; for a shared Discord channel it's the channel
>   (the harness has no user primitive — `core.SessionKeyFor` is deliberately not
>   user-scoped). A fire wakes that scope's canonical session. `list_intentions`
>   filters `ListWorkflowExecutions` on the **`IntentionUser` Search Attribute**
>   (`IntentionWorkflow` upserts `IntentionUser` / `IntentionKind` /
>   `IntentionState` on start + every transition). Registering the three on the
>   namespace is a deploy step; an unregistered upsert is a harmless no-op, but
>   the `list` query against one fails and surfaces (no fallback). Schedules
>   aren't workflow executions, so they keep the `intn-sched:<scope>:` id-prefix
>   filter.
> - The proactive seed is written **role=`user`, seq 0** (ClassifyRequest
>   requires it); `initiated_by` carries the real provenance.
> - `cron` and any daily cadence run in the **execution engine's timezone**
>   (per-user timezone is deferred — 2026-09-02).
> - **The genesis daily-review intention is NOT built, but has no blockers left:**
>   it's a plain `kind=schedule` intention (daily cron) that **the agent arms
>   itself** via `create_intention` when a routine makes sense (no auto-genesis —
>   the harness has no "onboard user" event and the bar for standing behaviour is
>   high). Its fired turn derives its own watermark — `MAX(turns.started_at)
>   WHERE initiated_by = 'intn:<scope>:daily-review'` — so "review everything
>   since I last reviewed" needs **no new table, column, or workflow kind**
>   (same "no watermark" stance as migration `011`). `importance` is computed by
>   the review turn from raw signals (`stop_reason`, correction counts, plan
>   outcomes), not stored.
> - No suppression/dedup state on the intention beyond `fired_count`; the
>   deciding turn's judgment + `lcm` are the only guard so far.
>
> Parent: [`../04-architecture-orchestrator-vision.md`](../04-architecture-orchestrator-vision.md).
> Builds on `coordinator.go` / `turn.go`, [`episode-lifecycle.md`](episode-lifecycle.md),
> [`request-pipeline/08-planning.md`](request-pipeline/08-planning.md) (the
> `PlanWorkflow`), [`lane-model.md`](lane-model.md),
> [`memory-slot.md`](memory-slot.md) (agent-brain = the belief/preference store).

### The reframe

**A proactive turn is the existing reason-act turn, started by a trigger the
agent set for itself instead of by a user message.**

Nothing downstream of "a message arrives at the `CoordinatorWorkflow`" changes —
same `ClassifyRequest`, same Lite/Deliberate fork, same episode / `PlanWorkflow`,
same `lcm` + memory, same delivery. The proactive turn's opening message is one
the agent wrote to itself:

```
[system, seq=0]  A travel email arrived: "Flight AA123 tomorrow delayed to 4:30pm."
                 Standing intention: "Notify me about travel-related email."
                 Decide whether and how to surface this now.
```

The earlier vocabulary (situation, policy, proactive decision, suppression,
feedback) collapses into machinery that already exists:

| concept | already is |
|---|---|
| **Situation** — what's happening now | what every turn does: the model calls tools mid-loop to check live state. No `AssembleSituation` activity. |
| **Policy** — when am I allowed to act | `MemoryRetrieve` + model judgment. agent-brain holds "no travel notifications", past "stop doing this" corrections, quiet hours. Not a rule engine. |
| **Opportunity detection** | a **default intention** seeded at genesis: *"periodically review recent episodes for anything worth raising."* Not a subsystem. |
| **Proactive decision** — act? | the deciding turn's own output: a message = act; ending silently (`no_tool_calls`, empty response) = suppress. No arbiter workflow. |
| **Plan / Execute** | `ClassifyRequest` on the seed → Lite (notify) or Deliberate (`PlanWorkflow`). The wake is a `seq=0` message; everything downstream is untouched. |
| **Suppression / cooldown** | the deciding turn's judgment (`lcm` shows what it already said) + the intention workflow's own state (it knows when it last fired). Not a cooldown subsystem. |
| **Feedback / adaptation** | already built: "stop these" → agent-brain correction → future deciding turns retrieve it and stay quiet. Skip. |

Research anchors still apply as *judgment* guidance, not as components: BDI
(Bratman; Rao & Georgeff) — an intention persists until its trigger fires or the
agent revises it, no re-scoring every pass; Horvitz "Principles of
Mixed-Initiative Interaction" (1999) — initiate on expected utility minus
interruption cost, with bounded deferral; Generative Agents (Park et al., 2023) —
the daily review turn generates its own salient questions over recent episodes.

### An intention is a workflow, not a row

Each intention is one `IntentionWorkflow` execution. Temporal's own machinery
*is* the state — there is **no `intentions` table** (same stance as "a database
table is just redundant data").

| intention concept | Temporal-native realization |
|---|---|
| identity | Workflow ID `intn:<user>:<slug>` — addressable, dedup, human-readable |
| objective + trigger spec | workflow **input args**; changed via a `revise` signal |
| "armed" | the workflow is **Running**, parked in a timer / poll loop / `Await` |
| "last fired" + fire count | workflow state, exposed by a `status` **Query** — and in event history |
| "satisfied" / one-shot done | the workflow **Completes** (`ExecutionStatus = Completed`) |
| "cancelled" | Temporal **cancellation** (`Canceled`) |
| "snoozed" | `snooze` signal extends the timer; still Running |
| "paused" | `Await(unpaused)` — Running but parked |
| list a user's intentions | `ListWorkflowExecutions` on Search Attribute `IntentionUser` (+ `IntentionKind`, `IntentionState`) — visibility **is** the registry |
| recurring ("every weekday 9am") | a **Temporal Schedule** whose action starts a one-shot `IntentionWorkflow` per firing — Temporal owns the calendar math, catch-up, pause |
| bounded history on a long-lived condition-watcher | `ContinueAsNew` per poll cycle |

### Trigger types → Temporal mechanisms

| trigger | mechanism |
|---|---|
| TIME `at 9am` | one-shot workflow: `workflow.Sleep(until)` → fire → complete |
| DEADLINE `30 min before flight` | `Sleep(event_time − offset)`; `event_time` from a calendar `call_tool` at arm time, recomputed if a `revise` says it moved |
| SCHEDULE `every weekday` | Temporal **Schedule** → one-shot `IntentionWorkflow` per occurrence |
| CONDITION `stock < X` | loop: `Sleep(poll)` → `CheckCondition` activity → met ? fire : `ContinueAsNew` |
| STATE CHANGE `calendar becomes free` | same loop; `CheckCondition` diffs against last-seen state held in **workflow state** |
| INACTIVITY `no reply for 2 days` | timer = 2d; the coordinator sends a `reset` signal on any user activity, restarting it; fires only if the timer ever completes |
| EVENT `email arrives` | poll variant for v1 (`Sleep(5m)` → `CheckForNewEmail`); push later = gateway webhook → `signal` the workflow |

`CheckCondition` is one generic activity: it runs the intention's declared probe
(`{tool, args}` through the existing tool-dispatch path) and compares the result
to the intention's declared predicate / threshold / last-seen value.

### The fire path

```
IntentionWorkflow trigger fires
   │
   ▼  FireIntention activity
SignalWithStart CoordinatorWorkflow(<user's session>)  { objective, why, intention_id }
   │
   ▼  Coordinator's Wake handler (sibling of NewMessage in the existing selector)
starts a TurnWorkflow, seed = the synthesized system message, initiated_by = "intn:<id>"
   │
   ├─ a turn is already active  ──▶  fold in: SignalExternalWorkflow into it
   │                                 { pending_mention, why } — the active turn's
   │                                 model places it (same path used for follow-ups)
   │
   └─ no active turn  ──▶  the deciding turn runs; if the user is offline the
                           coordinator starts headless and the turn routes its
                           output to the session's gateway channel
```

The deciding turn is a **normal turn**: `ClassifyRequest` (Lite notify vs
Deliberate `PlanWorkflow`), `MemoryRetrieve` (preferences, "stop doing this"
corrections, quiet hours — the "policy"), tool calls (the "situation" check),
then it either **produces a message** (act, delivered) or **ends silently**
(suppress). `FireIntention` gets the turn's outcome back; an `IntentionWorkflow`
that is suppressed N times in a row self-cancels and writes a belief
(*"user doesn't want X"*).

Global "don't fire three at once" falls out for free: simultaneous fires all
`SignalWithStart` the same per-session coordinator, which already serializes
turns, so each deciding turn sees in `lcm` what the last one just said.

### What's actually new

1. **`IntentionWorkflow`** — one workflow type (loop-worker): a timer / poll loop
   + `revise` / `snooze` / `cancel` / `reset` signals + a `status` query. The
   single new primitive.
2. **Search Attributes** `IntentionUser` / `IntentionKind` / `IntentionState` —
   `IntentionWorkflow` upserts them (identity two on start, state at every
   transition); `list_intentions` filters `ListWorkflowExecutions` on them, so
   visibility *is* the registry. Registering the three Keyword attributes on the
   namespace is the one deploy step.
3. **Coordinator `Wake` signal** — a handful of lines in the existing selector
   (`coordinator.go:102`), plus honouring `initiated_by` on the started turn.
4. **`turns.initiated_by` / `episodes.initiated_by`** — one column, a provenance
   string (`user` | `intn:<id>` | `plan`).
5. **Activities** (tenant-worker, hold a Temporal client — `ModelCall` already
   does): `ArmIntention` / `ReviseIntention` / `CancelIntention` behind agent
   tools; `FireIntention` (`SignalWithStart` the coordinator); `CheckCondition`
   (the generic probe runner for condition / state / event / inactivity).
6. **One genesis Schedule per user** — the daily "review recent episodes for
   anything worth raising" intention.

### The agent's tools

**Two model-facing tools** (`tool-registry.md`, "Resolved: Three-Layer Tool
Taxonomy" — 6 → 2, 2026-09-04): `create_intention` (its own schema — the one
genuinely complex operation) and `manage_intention(action, intention_id, …)`
with `action` in `{list, inspect, revise, snooze, cancel}` — CRUD on one
construct, not five distinct intents. Both call into `tools_intention.py`,
which keeps the five operations as separate internal functions (`list`/`inspect`
are `ListWorkflowExecutions` / `query_workflow`); `manage_intention` is a thin
dispatcher over them. The agent's currently-armed intentions render into its
prompt as a small block (like the plan), so it reasons about its commitments
and prunes stale ones.

### No per-user holder

Each `IntentionWorkflow` arms its own trigger, so there is nothing for a parent
to hold; Temporal visibility replaces "the list". Cross-intention coordination is
the per-session coordinator's existing turn-serialization plus the deciding
turn's judgment. A per-user holder would only add one place for global
proactivity policy — promote it later if memory + the coordinator choke point
prove insufficient.

### Delivery — no fallback

One target: the channel the session already uses (Discord → the bot posts; web →
Postgres, surfaced on next open), via the existing `deliver:{platform}:{connection}`
activities. Delivery failure fails the turn and is surfaced. No channel fallback
— same stance as the rest of the system.

### Data model

| thing | where |
|---|---|
| intentions | Temporal — `IntentionWorkflow` executions + Schedules. **No table.** |
| intention provenance on work | `turns.initiated_by` / `episodes.initiated_by` (one column) |
| a proactive turn's seed | a `system`-role `messages` row |
| preferences / quiet hours / "stop doing X" | agent-brain memory (already the store) |
| engagement feedback | agent-brain memory (a deciding turn observing the follow-up writes it — same loop as skill confidence) |

### Degradation (no fallback)

- An `IntentionWorkflow`'s `CheckCondition` errors → Temporal retry; a persistent
  failure surfaces the intention as failed, it is not silently dropped.
- The deciding turn errors → Temporal retry → `failTurn`; the intention re-fires
  on its next trigger.
- Delivery fails → the turn fails, surfaced.

### Deferred

- **Push EVENT triggers** — gateway webhook ingestion → signal the workflow.
  Start with polling.
- **A per-user proactivity-policy holder** — start with memory + the coordinator
  choke point.
- **Learned importance weights** for the daily review — start with the model
  judging.
- **Digests / batching** low-urgency items — start with individual turns.
- **Cross-session reach** (fire against a user with no live session) — the
  headless-coordinator path is sketched, not designed.

### Open Questions

- **Genesis** — what creates a user's daily-review Schedule, and when? (Same
  question `CoordinatorWorkflow` has for its own first start.)
- **`initiated_by` and the active-turn fold-in** — when a wake arrives mid-turn,
  fold as a priority the model surfaces next step, or cancel the in-flight
  `ModelCall` (the interrupt path)? Lean fold unless the intention is
  time-critical.
- **Quiet hours before they're learned** — until agent-brain has the fact, the
  deciding turn is conservative and an early proactive message asks
  *"when's off-limits, how should I reach you?"*
- **`CheckCondition` cost** — a frequently-polled NL predicate check is a real
  cost. Cache the last result and only re-judge on a raw-value change, or
  constrain predicates to a small expression grammar over the probe result.
- **Slug collisions / intention identity** — `intn:<user>:<slug>` where the slug
  comes from the objective; how is it derived, and what happens when the agent
  arms a near-duplicate (revise the existing one, or run both)?
