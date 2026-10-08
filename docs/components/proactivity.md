# Component: Proactivity — Waking

> STATUS: DESIGN v4 (2026-10-08). Supersedes v3 ("intentions"), which is
> **removed**. v3 built user requests as one primitive — `IntentionWorkflow`, one
> Temporal execution per intention, with the workflow's own state *being* the
> record ("an intention is a workflow, not a row"). Four things were wrong with
> it, and the build went with them:
>
> 1. **The abstraction didn't cover its own cases.** Recurring intentions were
>    Temporal *Schedules*, which are not workflow executions — so `list` needed
>    two code paths (Search Attribute filter for workflows, id-prefix filter for
>    schedules) and `revise`/`snooze` "don't apply" to schedules, returning a note
>    instead of doing the thing.
> 2. **Conditions were structurally expensive AND non-functional.**
>    `CheckCondition` ran a tool probe **plus an LLM-judged natural-language
>    predicate** every poll, so cost scaled with `intentions × frequency` to answer
>    questions a comparison settles. It was also a stub: it always returned
>    `fired=False`, so `condition`/`state`/`event` intentions *never fired at
>    all* — v3's headline capability was dead code.
> 3. **Identity was deferred, and it is the whole feature.** `intn:<user>:<slug>`
>    with the slug derived from free text (a `re.findall` over the objective), and
>    near-duplicate handling left as an open question. "Don't arm the same thing
>    twice" is the thing users notice; a slug is a guess where a name is a
>    decision.
> 4. **Most of it was already solved.** A direct notification at 9am is a tool
>    call to an external system the agent already reaches through mcp-hub
>    connections. Only *waking the agent's own reasoning* needs the harness.
>
> Removed with v3: `IntentionWorkflow` + its 5 tests, `FireIntention`/
> `CheckCondition` activities, `tools_intention.py`, the `IntentionUser`/
> `IntentionKind`/`IntentionState` Search Attributes **and their namespace
> registration deploy step**, and `IntentionInput`/`IntentionStatus`/`ProbeSpec`.
> `turns.initiated_by` (migration `020`) stays; the provenance prefix is now
> `wake:` rather than `intn:`.

> Parent: [`../04-architecture-orchestrator-vision.md`](../04-architecture-orchestrator-vision.md).
> Builds on `coordinator.go` / `turn.go`, [`turn-pipeline.md`](turn-pipeline.md),
> [`memory-slot.md`](memory-slot.md) (agent-brain = the belief/preference store),
> [`tool-registry.md`](tool-registry.md).

### The reframe

**An intention was a noun the harness never needed. A wake is a verb it already
knows how to perform.**

The axis that matters is not "time-based vs condition-based" — it is **who acts at
the future moment**:

| the user asked for | who executes at T | mechanism |
|---|---|---|
| "remind me to pay bills on the 1st" | an external system | **that system's own tool** — the agent already has it |
| "let me know when my weight drops below 70kg" | an external system watching its own data | same |
| "every Sunday, review my week and tell me what to change" | **the agent's own reasoning** | a wake |
| "check on this in three days and decide if it still matters" | **the agent's own reasoning** | a wake |

No external system can wake *your* agent. That is the entire gap, and it is one
`SignalWithStart` the harness already makes. Everything else is a notification,
and notifications are a solved problem the agent reaches through connections.

**The cold reading of v3 is that a large part of it reimplemented a capability the
agent already had, behind a new name.** This document keeps the part that was
genuinely new — the wake — and deletes the rest.

### The selection rule

This is the whole design in one sentence, and it belongs in the prompt:

> If an external system should act at T, use that system's tool.
> If the agent should *think* at T, arm a wake.
> If the agent should think when something changes, arm a watch.

A **watch** is not a separate construct: it is a wake whose decider calls a tool to
check something. The check is a tool call inside the turn it woke, so there is no
poll-and-judge loop, no fast-tier judge, and no cost that scales with frequency —
the agent widens its own interval when a watch proves noisy.

### A wake is a Schedule, not a workflow to keep alive

A time wake is a **Temporal Schedule**. The Schedule *is* the record: created,
listed, described, retimed, paused, resumed and cancelled through Temporal's own
schedule APIs, with calendar maths, catch-up windows and overlap policy already
solved. **There is no wakes table**, and therefore nothing to keep in sync with
one.

| wake concept | Temporal-native realization |
|---|---|
| identity | Schedule ID `wake:<scope>:<name>` — the agent supplies `name`, so dedup is a decision, not a slug guess |
| cadence | `ScheduleSpec` — `cron_expressions` (UTC), `intervals`, or `start_at`/`end_at` |
| one-shot | the same Schedule with `state.remaining_actions = 1` — one mechanism, one listing path |
| "armed" / "paused" | `ScheduleState.paused` |
| fire count / last fire / next fire | `ScheduleInfo.num_actions` / `recent_actions` / `next_action_times` |
| the session it wakes | in the action's args, not a column |
| overlap | `SchedulePolicy(overlap=SKIP)` — two wakes stacked on one session is never wanted |

A Schedule's action starts **`WakeWorkflow`** — a workflow whose entire body is one
`WakeSession` activity call and a completion. It carries no state, takes no signals
and answers no queries, so a wake costs a handful of history events rather than a
durable per-commitment execution, and there is nothing to bound with
`ContinueAsNew`.

### The fire path

```
Schedule tick (or an event, later)
   │
   ▼  WakeWorkflow → WakeSession activity
SignalWithStart CoordinatorWorkflow(<session>)  { wake_id, objective, why }
   │     WorkflowIDConflictPolicy.USE_EXISTING — running or not is not a question
   ▼  Coordinator's Wake handler (sibling of NewMessage in the existing selector)
starts a TurnWorkflow, seed = the synthesized system message, initiated_by = "wake:<id>"
   │
   ├─ a turn is already active  ──▶  fold in: SignalExternalWorkflow into it
   │                                 { pending_mention, why } — the active turn's
   │                                 model places it (same path used for follow-ups)
   │
   └─ no active turn  ──▶  the deciding turn runs; if the user is offline the
                           coordinator starts headless and the turn routes its
                           output to the session's gateway channel
```

**"The session may be running, or terminated" is not a special case** — it is what
`SignalWithStart` does, and it is what `loop-worker/cmd/starter` has always done.
Barge-in was already built.

The deciding turn is a **normal turn**: the model calls `recall` (preferences,
"stop doing this" corrections, quiet hours — the "policy") and other tools (the
"situation" check) as it judges it needs to, then it either **produces a message**
(act, delivered) or **ends silently** (suppress).

### Event wakes — designed, not built

A wake can also be armed on an event. The pieces, and where they belong:

- **mcp-hub owns the public hop.** It already holds connection credentials and
  already has a public callback path (it does the OAuth callbacks today), so it
  registers the provider webhook, receives the event, normalizes it, and POSTs
  *internally* to the harness. The harness therefore grows **no unauthenticated
  public endpoint**.
- **The callback carries an opaque signed token**, not a session key:
  `base64(session_key | expiry) + HMAC`. mcp-hub treats it as opaque and never
  learns what a session is; the harness verifies it and reads the session key out,
  so the harness needs **no mapping table either**.
- **Revocation needs no revocable token**: cancelling deletes mcp-hub's
  subscription row (and unsubscribes from the provider), so no further callbacks
  arrive.
- **Order of operations is harness-first**: arm the wake and get the callback,
  *then* have the agent call the external system's own subscribe tool. The endpoint
  exists before anyone is told about it, so an event cannot arrive early. The
  orphan window is on the harmless side — a Schedule that expires unfired.
- **Idempotency is required**: the callback must dedupe on event id, or a retried
  POST wakes the agent twice.

**Why it is not built yet:** most MCP servers are request/response and expose no
subscribe capability at all; real push (Calendar, say) is a provider-API feature
needing webhook domain registration. So this path depends on connectors in
mcp-hub, which is mcp-hub work. `arm_wake` deliberately **does not offer
`on_event`** until then — a tool the model can call that cannot work is worse than
a tool it cannot see. The interim answer is a periodic wake with a cheap tool-call
check, which works against any MCP server today.

### The agent's tools

**Two model-facing tools** (`tool-registry.md`, "Resolved: Three-Layer Tool
Taxonomy" — the same create-plus-dispatcher consolidation v3 used, so only the
construct changed):

- **`arm_wake`** (`tools_wake.py`) — its own schema, the one genuinely complex
  operation. `name` is required and is the identity.
- **`manage_wake(action, wake_id, …)`** with `action` in
  `{list, inspect, revise, cancel}` — a thin dispatcher over the four handlers.

**Why `arm_wake` and not `create_schedule`:** external tool names arrive
dynamically through `discover_tools`, and mcp-hub's disambiguation covers collision
*within its own tier* only — a native tool colliding with a discovered
Calendar/Reminders tool has no resolution path. `schedule`, `reminder`,
`notification`, `event`, `calendar`, `timer` are exactly the words such a server
claims. `Wake` is already this codebase's own vocabulary (`WakeSignalName`,
`WakeSessionActivity`); `arm` is nobody's.

The agent's armed wakes render into its prompt so it reasons about its own
commitments and prunes stale ones.

### Data model

| thing | where |
|---|---|
| time wakes | Temporal — Schedules. **No table.** |
| event wakes | mcp-hub's subscription table (opaque token, source, filter, callback, expiry, last event id) — mcp-hub's data, in mcp-hub's own database |
| a proactive turn's seed | a `system`-role `messages` row |
| wake provenance on work | `turns.initiated_by` = `"wake:<id>"` (one column) |
| preferences / quiet hours / "stop doing X" | agent-brain memory |
| engagement feedback | agent-brain memory |

### Degradation (no fallback)

- A `WakeSession` that cannot reach its session fails the `WakeWorkflow` visibly.
  The Schedule's next tick is the retry, and a Schedule that keeps failing shows up
  in its own action history.
- The deciding turn errors → Temporal retry → `failTurn`; the wake fires again on
  its next tick.
- Delivery fails → the turn fails, surfaced.

### Deferred

- **Event wakes end to end** — mcp-hub's subscription table and provider
  registration, the harness `/events` ingress, and `on_event` in `arm_wake`. See
  above; the design is settled, the dependency is another repo.
- **Headless wake with no live session** — confirmed working, deliberately planned
  later.
- **Digests / batching** low-urgency wakes — start with individual wakes.
- **A client surface for wakes.** Deliberately none: Retro is the record of what
  you *did*, not the agent's to-do list, and external reminders are visible in the
  external apps where the user already sees them.

### Open Questions

- **`list` unions one source today, two later.** Right now every wake is a
  Schedule, so `list_wakes` is one prefix filter. With event wakes it becomes a
  union of Temporal's schedule list and mcp-hub's subscriptions — the one place
  the abstraction leaks, confined to one function.
- **Suppression counting moved out of the workflow.** v3 let an intention that was
  suppressed N times self-cancel and write a belief, because the workflow counted.
  A Schedule does not count suppressions, so that judgment has to move to the
  deciding turn (or agent-brain). Not yet designed.
- **Quiet hours before they're learned** — until agent-brain has the fact, the
  deciding turn is conservative and an early proactive message asks *"when's
  off-limits, how should I reach you?"*
- **When does a wake stop being worth it?** A weekly review nobody reads should
  eventually be pruned. The agent can see its own wakes and cancel them, but
  nothing yet prompts it to look.
