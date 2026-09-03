# Component: Proactivity — Intentions, Deliberation & Mixed-Initiative

> STATUS: DESIGN (2026-09-02, rewritten around first-class components). Not built.
>
> Removes the asymmetry that only the user can start a conversation. The agent
> is a peer: it has its own **intentions** — for its benefit, the user's, or
> both — decides when to act on them, and can initiate.
>
> Parent: [`../04-architecture-orchestrator-vision.md`](../04-architecture-orchestrator-vision.md).
> Builds on [`episode-lifecycle.md`](episode-lifecycle.md),
> [`lane-model.md`](lane-model.md), [`skill-subsystem.md`](skill-subsystem.md),
> [`memory-slot.md`](memory-slot.md) (agent-brain is the belief store),
> `coordinator.go` / `turn.go`.
>
> **Relationship to memory mining.** agent-brain already runs its own
> consolidation pipeline (`dreaming.md`, superseded — mining + EMU
> construction/lifecycle) that turns raw transcripts into memory. Deliberation
> here is **not** a second mining pipeline: it *reads* agent-brain's output as
> one belief source, alongside live system state, and its job is to form
> **intentions**, not memories. It writes back only higher-level reflection
> insights (a `reflection` kind).

### Role

When there is something worth a conversation — the agent finished a background
task, a watched deploy failed, a fix it applied three days ago should be
checked, it has a question it needs answered to do future work well, it noticed
a pattern worth confirming — the agent starts that conversation itself, at a
moment it judges appropriate, through the channel the session already uses.

### The frame: Belief–Desire–Intention

A named cognitive architecture (Bratman; Rao & Georgeff; AgentSpeak/Jason), and
the model this design converged on:

| BDI | here |
|---|---|
| **Beliefs** | agent-brain memory + live system state (episodes, skill confidences, plan ledgers, unanswered questions, deferred work) |
| **Desires** | things the agent would like — surfaced by reflection |
| **Intentions** | desires it has committed to, armed as running `IntentionWorkflow`s / Temporal Schedules |

Deliberation cycle: update beliefs → reflect → form / revise / retire
intentions → (intentions fire themselves) → *reconsider* when one fires.

Two BDI results carry weight:
- **Commitment strategy** — an intention persists until its trigger fires or the
  agent explicitly revises it; the deliberation loop does *not* re-score every
  intention every pass.
- **Intention reconsideration** (Kinny & Georgeff) — a cheap "*should* I
  reconsider?" check gates expensive re-deliberation. Here: a fired intention's
  reflection turn runs a fast-tier "still relevant?" pass first, escalating only
  if it decides to act.

Other influences: **Generative Agents** (Park et al., 2023) — importance-gated
reflection, self-generated salient questions, insights that become durable
beliefs; **Horvitz, "Principles of Mixed-Initiative Interaction" (1999)** — the
decide-whether-to-initiate problem as expected utility with explicit
uncertainty about the user's goals, and **bounded deferral**; **Value of
Information** — an info-gap intention (the agent's own ask) is justified only if
the expected improvement to future work exceeds the cost of asking.

### The decision: first-class harness components, not an agent-authored worker

An earlier draft had the agent author and deploy its own Temporal worker for
intentions (a `software-engineering.md` capability). Rejected as redundant: it
builds a project workspace + deploy pipeline to produce a handful of workflow
types the harness can hand-write, and asks the agent to re-derive Temporal
patterns the harness already embodies.

**Instead: proactivity is a small set of first-class components — workflows and
activities deployed with the harness, exactly like `ClassifyRequest` /
`ComposeSkill` / `RoutingWorkflow` / `SkillSynthesisWorkflow`.** The agent does
not author their code. It *commands* them through meta-tools, the same way it
invokes `spawn_subagent` or `search_tools`. The dynamic, agent-owned part is the
**judgment** — which intention to arm, with what parameters, when to revise or
drop it — not the control flow.

The variety of intentions is smaller than it looks: every one decomposes into
*"at time T / after delay D / when condition C, run a reflection turn about
purpose P."* The control-flow shapes are three; the variety lives in **P** (a
natural-language purpose) and **C** (a watch condition given as parameters).

If a genuinely novel intention *shape* is ever needed, that is a normal harness
change (add a workflow type) — or, once `software-engineering.md` exists for its
own reasons, the agent extends the component then. Not a reason to build that
capability first.

### The components

#### `IntentionWorkflow` — the generic durable intention

Input: an **intention spec** (data, not code):

```
IntentionSpec = {
  kind:     "delay" | "watch",
  purpose:  str,            # natural language — handed to the reflection turn
  # kind == "delay":
  after:    duration,
  # kind == "watch":
  watch: {
    probe:    { tool, args },   # what to check
    predicate: str,             # NL predicate, evaluated cheaply
    every:    duration,         # poll interval
    fires:    "once" | "each",  # one-shot, or on every occurrence
    expires:  duration | null,  # give up after
  },
}
```

- `kind == "delay"` → `workflow.Sleep(after)` → dispatch a reflection turn.
- `kind == "watch"` → loop: `EvaluateWatchCondition` activity (probe + predicate)
  every `every`, with `workflow.Sleep` between; on match → dispatch a reflection
  turn (and stop, unless `fires == "each"`); stop at `expires`.
- Long-lived until it fires/expires/is dropped. Accepts a `revise` signal (swap
  the spec) and a `snooze` signal (re-arm the timer).

**Cron intentions are a Temporal Schedule**, not an `IntentionWorkflow` — the
meta-tool creates a `Schedule` whose action starts a reflection turn with the
purpose. Schedules already give overlap/catchup/jitter/pause for free.

#### `DeliberationWorkflow` — the long-lived reflection loop

Per session (workflow id derived from the session key), first-class and
hand-built like `CoordinatorWorkflow`. Not something the agent arms — it is
started at a session's first contact and lives alongside the coordinator.

- Receives **`importance_event`** signals (below), accumulates a score in
  workflow state.
- Fires a reflection turn (`purpose = "deliberate"`) when the score crosses `T`
  (then resets), and on a slow internal timer (daily catch-all).
- Survives coordinator idle-exits; a fresh coordinator re-attaches. (Open
  question: exact lifetime — session- or user-scoped.)

#### reflection turn = `TurnWorkflow` with `ParentType == "intention"`

Not a new workflow type — the third `ParentType` alongside `"session"` and
`"turn"` (subagent). Seed is a `system`-role message: the intention's purpose +
any context the firing workflow gathered. Runs the normal reason-act loop
(Lite or Deliberate per the seed's shape). What the loop does:

1. **Reconsideration gate** (fast tier): "is this still relevant given what has
   changed?" Stale → drop, turn ends. Otherwise →
2. **Judgment** (Horvitz): act? how? Considers value (agent / user / both),
   P(good timing) — user reachable, turn active, quiet hours — and interruption
   cost. Produces an **urgency tier**:

   | tier | behaviour |
   |---|---|
   | **urgent** | interrupt — fold into the active turn as a priority follow-up (proactive turn if none active) |
   | **raise** | proactive turn if no active conversation; fold into the active turn if there is one |
   | **mention** | fold into an active turn *only*; if no conversation, **re-defer** (bounded deferral) and wait for the user |
   | **drop** | not worth it |

3. If it acts by initiating → the turn produces a message, delivered outbound
   through the session's existing channel. It becomes a normal conversation — a
   user reply is normal inbound to the **same session**, `build_conversation`
   has the proactive message as context, and if it leads to work it opens an
   episode.

For a `Deliberation` reflection turn, step 2 is instead: generate the *N most
salient questions* about what changed (Generative Agents) → answer from beliefs
→ per answer, **arm / revise / drop** an intention (via the same meta-tools) or
**write a belief** to agent-brain. It contacts the user only if an intention it
forms is `urgent`.

#### `ManageIntention` activity — the meta-tool backend

One activity behind the agent's meta-tools, wrapping the Temporal client (an
activity may hold one — `ModelCallActivity` already does for streaming):

| tool | op |
|---|---|
| `arm_intention(spec)` | start `IntentionWorkflow` / create `Schedule` |
| `list_intentions()` | list running `IntentionWorkflow`s + Schedules for this session |
| `revise_intention(id, spec)` | `signal_workflow` / update schedule |
| `snooze_intention(id, until)` | `signal_workflow` / pause schedule |
| `drop_intention(id)` | `terminate_workflow` / delete schedule |
| `inspect_intention(id)` | `query_workflow` |

Every armed intention is **rendered into the agent's context** — a "current
intentions" block, like the plan ledger — so the agent reasons about its
commitments and prunes.

#### `EvaluateWatchCondition` activity

Called by an `IntentionWorkflow` of `kind == "watch"`: run the `probe`
(`tool` + `args`, via the existing tool-dispatch path), evaluate `predicate`
against the result (a cheap fast-tier call — "does `<predicate>` hold given
`<result>`?" → bool). Returns `{fired: bool, note: str}`.

#### The importance signal

No new subsystem — folded into the paths that already run at the relevant
moments. `CompleteEpisode` / `CloseSessionEpisodes` / the turn-end block emit an
`importance_event` signal to the session's `DeliberationWorkflow`, with a
deterministic score:

| signal | weight |
|---|---|
| `episode.close_reason == 'superseded'` / `outcome == failure` | +3 |
| `episode.required_correction == true` | +2 |
| `stop_reason ∈ {max_iterations, max_retries}` (the agent struggled) | +2 |
| a composed skill's `confidence` dropped > δ this turn | +2 |
| an assistant question with no user reply after N turns | +1 |
| a reconciliation trigger fired | +1 |

#### The fold-in mechanic

When judgment is `mention` or `urgent` and a turn is active:
`SignalExternalWorkflow` into the active `TurnWorkflow` with
`{pending_mention: content, urgency}` — the *exact* mechanism the coordinator
already uses to forward follow-up messages. The active turn's next `ModelCall`
sees *"you have something to surface when it fits: X"*. **The active turn's
model decides placement** — it has the live conversation; the reflection turn
does not.

### Delivery — direct, no fallback

One target: the channel the session already uses. Discord → the bot posts.
Web → the message lands in Postgres, surfaced on next open. If delivery fails,
the intention fails, it is surfaced, the cause is fixed. No channel fallback —
same stance as the rest of the system.

### The initiative governor

The real governor is **judgment quality, improved by feedback**: the agent
observes engagement in the transcript (reply / act / "stop") → writes it to
memory (*"user valued the Friday heads-up"* / *"user muted deploy notes"*) →
future judgments read it. Same loop as skill confidence.

**Floors** (deterministic, minimal):
- a hard **daily cap** on agent-initiated turns;
- the judgment prompt anchored: *"the bar for initiating is high — a peer who
  interrupts constantly is worse than one who waits";*
- **quiet hours** — a learned user-world fact; until learned, judgment is
  conservative (only `raise` / `urgent`), and an early proactive message asks
  *"when's off-limits, how should I reach you?"*

### Data model

| thing | where |
|---|---|
| intentions (instances) | Temporal — running `IntentionWorkflow`s + Schedules. **No table**; `list_intentions()` queries Temporal. |
| beliefs / reflection insights | agent-brain memory (`reflection` kind) |
| `DeliberationWorkflow` accumulator | its own Temporal state |
| deliberation watermark | `sessions.last_deliberated_at` (new column) |
| turn provenance | `turns.initiated_by` — `'user'` \| `'agent'` \| `'intention'` (new column) |
| a proactive turn's seed | a `system`-role `messages` row (the purpose) |
| engagement feedback | agent-brain memory (written by a reflection turn observing the follow-up) |

### Temporal shape

| unit | where | cadence | does |
|---|---|---|---|
| `DeliberationWorkflow` | loop-worker, long-lived, per session | importance-threshold or daily | reflect → arm/revise/drop intentions, write beliefs |
| `IntentionWorkflow` | loop-worker | per spec (`delay` / `watch`) | fire → dispatch a reflection turn |
| cron intention | Temporal Schedule | per the agent's cron | start a reflection turn |
| `importance_event` | emitted from the existing episode-close / turn-end paths → `signal_workflow` | per event | accumulate into `DeliberationWorkflow` |
| reflection turn | `TurnWorkflow` (`ParentType == "intention"`), started via the coordinator | per fired intention | reconsideration → judgment → act / defer / drop |
| `ManageIntention` | tenant-worker activity (holds a Temporal client) | per meta-tool call | arm / list / revise / snooze / drop / inspect |
| `EvaluateWatchCondition` | tenant-worker activity | per watch poll | probe + predicate → `{fired, note}` |
| fold-in | `SignalExternalWorkflow` into the active turn | `mention` / `urgent` + turn active | hand the pending mention to the live model |
| delivery | existing `deliver:{platform}:{connection}` activities | per proactive turn | post to the session's channel; no fallback |

### Degradation

No fallback. `DeliberationWorkflow` down → no reflection, a visible failure. A
reflection turn errors → surfaced; the intention re-fires or the agent catches
it at the next deliberation.

### Deferred

- **Novel intention shapes** beyond `delay` / `watch` / `cron` — add a workflow
  type when a real one appears (or via `software-engineering.md`, decoupled).
- **Learning the importance weights** — start hand-tuned.
- **Digests / batching** `raise`-tier items — start with individual turns.
- **Web push** — start with surface-on-open.
- **Cross-session deliberation** — one `DeliberationWorkflow` per session.
- **VoI as an explicit calculation** — start with the model judging.

### Open Questions

- **Coordinator's proactive-start path** — a fired intention starts a turn
  against a session. `SignalWithStartWorkflow` with an
  `{initiated_by: "intention", purpose, context_ref}` payload (reusing the
  existing coordinator entry point) is the natural fit — the active-turn guard
  and reply continuity both argue for going through the coordinator.
- **`DeliberationWorkflow` lifetime** — session- or user-scoped? What starts it
  (coordinator genesis?), and does it outlive coordinator idle-exits or
  re-attach?
- **Genesis** — does the agent start with zero intentions and the
  `DeliberationWorkflow` is the only always-on piece, or is a "check in with the
  user occasionally" intention seeded?
- **`urgent` + active turn** — fold as priority (model surfaces it next step) or
  cancel the in-flight `ModelCall` (the interrupt path)? Lean toward the former
  unless time-critical.
- **`EvaluateWatchCondition` predicate** — a fast-tier NL predicate check each
  poll is a real cost for a frequently-polled watch. Cache the last result;
  only re-evaluate on change? Or constrain `predicate` to a tiny expression
  grammar over the probe result?
