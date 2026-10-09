# Component: Loop Taxonomy — labels as agent-loop shapes

> STATUS: **DESIGN DRAFT (2026-10-08). NOTHING BUILT.** This doc captures the
> concept agreed in session: the classification label for a request is **the
> skeleton of the high-level process that achieving it demands** — an
> agent-loop shape. It is the taxonomy a classifier (e.g. a fine-tuned laya)
> would be trained on, and the vocabulary a future loop-router would switch on.
>
> REVISED (2026-10-08, same day): converged with the owner under the
> **split-first** rule (below) — `diagnose`/`research` split confirmed;
> `learn` split from `tend`; `acquire` split from `arrange`; `react` stays
> a label (not an urgency modifier). Current set: **eleven labels** plus
> `monitor`; the operational entry criteria and boundary drills are drafted
> below (same day).
>
> This deliberately revises [`turn-pipeline.md`](turn-pipeline.md)'s "no
> classifier deciding a lane" stance. That removal was right about the shape
> of the thing it removed (a classifier of *topics*/lanes feeding a
> model-adjacent pre-pipeline); this proposal differs in four ways, stated
> here so the departure is conscious, not drift:
>
> 1. **Labels are loop skeletons, not topics or difficulty.** Two requests
>    share a label only if they demand the same phases, iteration driver,
>    exit criterion and rails.
> 2. **The classification is cheap and local** (a ~0.2–0.5 s CPU forward pass,
>    measured — see `~/Documents/laya-experiment/`), not a model call.
> 3. **The fallback is safe by construction.** `conversation` is the default
>    and today's whole loop; a wrong label costs at most an unhelpful profile,
>    and "not sure" resolves to conversation.
> 4. **A shape is a profile, not a forked workflow** — prompt sections,
>    scratchpad templates, provisioning and a small number of deterministic
>    rails on the *existing* `TurnWorkflow`, never a parallel workflow family
>    (the lesson of the workflows this project deleted).

### Role (one line)

Define the closed set of task labels a personal assistant's requests collapse
into, where each label is an **agent-loop shape** — the process skeleton a
task of that kind must follow — so a request can be routed to the loop
profile that serves it.

### The concept

A task type is not what the task is *about*; it is **how the work must
proceed**. "Book me a table" and "plan the Bali trip" are different topics
but the same demanded process: elicit constraints, find options, compare,
decide, commit, confirm — one shape (`arrange`). "Why is my gateway
restarting" and "why is my sleep data wrong" are different systems but the
same process: hypothesize, gather evidence, eliminate, verify — one shape
(`diagnose`).

The rule for drawing labels:

> Two requests get the same label **iff** they demand the same loop
> skeleton — the same phases, the same thing that drives another iteration,
> the same exit criterion, the same verification of "done". Subject matter,
> difficulty, and model tier are irrelevant to the label.

Corollaries:

- **Domains collapse into shapes, many-to-one.** Finance questions, wardrobe
  chat, memory lookups are all `conversation` with different tool
  provisioning. Domain-specific behaviour rides *underneath* a shape as
  profile config, not as its own label.
- **The label set is small and near-exhaustive.** Unlike a topic taxonomy
  (open-ended, every life domain a new label), loop shapes in a personal
  assistant are a handful — and the hardest working one (`conversation`) is
  the safe fallback, so classification failure degrades to today's behaviour.
- **Shapes are the entry point, not the whole journey.** A `react` loop may
  hand off into `diagnose`; a `tend` review may spawn an `arrange`. The
  classifier picks the *first* shape; handoffs are the running loop's own
  business.
- **Split-first (owner's rule, 2026-10-08).** When two candidate shapes
  differ on *any* of the axes below — even "only" in maintained state or
  exit criterion — they get **separate labels**. Merging two shapes under
  one label is the exception and needs an argument. The axis table
  therefore functions as the split test, not a merge test.

### The axes that define a shape

Two candidate labels may merge only if they agree on these. They are also the
spec a loop profile implements.

| Axis | The question it answers |
|---|---|
| **Entry trigger** | What kind of request/situation enters this shape? |
| **Maintained state** | What durable structure does the loop keep across iterations? (ledger, checklist, outline, history — or nothing) |
| **Iteration driver** | What sends it around the loop again? (information gain, uncovered gaps, drift, waiting on others, the user's turns, unfolding events) |
| **Determinism** | What is a template/rail the harness guarantees vs. what the model steers? |
| **Exit criterion** | When is it over? (answered, cause verified, coverage met, thing committed, procedure accepted, next review scheduled, user led away, stabilized) |
| **Verification of done** | Whose word ends it — the model's claim, a check result, a third party's confirmation, nobody's? |
| **Time behaviour** | One burst / dormant-awaiting (resumes on wakes, events, replies) / long-horizon scheduled |
| **Commit & approval points** | Where does it change the world, and what gates that? |

### The label set (v1)

Eleven labels for message-initiated turns, plus `monitor` as an existing
engine-owned shape that is not message-classified.

---

**`conversation`** — *the default; the shape the harness runs today.*

- **Entered when**: no other shape is clearly demanded — questions, advice,
  lookups, small asks, casual exchange.
- **Skeleton**: model-steered reason-act iterations; recall/tools on demand;
  produce a reply or a small direct action.
- **Iterates on**: the model's own judgment that more work is needed.
- **Exits when**: answered / blocked on the user / budget ceiling.
- **State**: none beyond the transcript (scratchpad if the model chooses).
- **Verification**: the user's acceptance.
- **Rail**: it *is* the fallback — any classification result may land here
  at no structural cost.
- **Personal-assistant examples**: "why did we pick Postgres", "how much did
  I spend on food last month", "what was my food spend".

---

**`diagnose`** — *find the cause.*

- **Entered when**: the goal is causal — find why something the user runs or
  owns is malfunctioning, anomalous, or wrong. ("Why is X broken/slow/wrong")
- **Skeleton**: frame the symptom → form hypotheses → pick the investigation
  with the highest expected information gain → collect evidence → eliminate
  or confirm → verify the cause → report (fix proposal optional).
- **Iterates on**: information gain — **each pass must add evidence or
  eliminate a hypothesis** (the anti-thrash invariant; a pass that does
  neither fails the loop loudly).
- **Exits when**: the cause is verified, or an impasse is declared visibly
  after K cycles.
- **State**: hypothesis ledger + evidence log (a pinned scratchpad template,
  not free-form prose).
- **Verification**: the cause must be reproduced/confirmed before submit —
  a diagnosis is never delivered on plausibility alone.
- **Time behaviour**: one burst, usually.
- **Examples**: "why is my gateway restarting", "why did this transaction
  get miscategorized", "why is my laptop so hot".

---

**`research`** — *build understanding.*

- **Entered when**: the user wants knowledge assembled from sources to
  understand or decide; the ask is a question about the world, not a
  malfunction and not a commitment. Breadth-shaped.
- **Skeleton**: scope the question → broad gather (parallel subagents
  legitimate here) → outline/synthesis draft → **gap check against scope** →
  drill the gaps → final synthesis artifact.
- **Iterates on**: uncovered gaps in the outline.
- **Exits when**: coverage criteria met; the synthesis is delivered as an
  artifact (document, not a chat answer).
- **State**: coverage outline + source notes.
- **Verification**: sources tracked per claim; the user's judgment.
- **Time behaviour**: one burst, occasionally resumable.
- **Examples**: "EV leasing vs buying, work it out for me", "what does the
  literature say about creatine", "how did my neighborhood change".

---

**`arrange`** — *get something arranged or committed.*

- **Entered when**: the outcome is a real-world arrangement or reservation
  with external parties — trips, events, bookings, meetups, signups.
- **Skeleton**: elicit constraints (ask_user freely) → generate/find options
  → evaluate against the constraint ledger → converge → user decides →
  **commit** (book/reserve) behind approval gates → track until confirmed
  (reschedule loops, waiting-on-others).
- **Iterates on**: unresolved constraints; waiting states; changes from the
  other parties or the user.
- **Exits when**: the thing is committed and confirmed — or abandoned.
- **State**: constraint ledger + option comparison + waiting-on list.
- **Verification**: third-party confirmation (a booking number, not a
  "should work").
- **Time behaviour**: **dormant-awaiting is first-class** — the loop parks
  on "waiting for the restaurant to confirm" and resumes on an event/wake.
  Nothing in today's harness does this.
- **Commit & approval**: a normal path that changes the world; approval
  gating sits on every commit.
- **Examples**: "book me a table Saturday", "plan the Bali trip", "find a
  dentist and book something".

---

**`acquire`** — *get a thing, and make sure it arrived right.*

- **Entered when**: the outcome is possession of a purchasable good —
  buy a specific/needed thing, order a replacement, restock.
- **Skeleton**: establish the need/spec (may be inferred from context) →
  search the market (existing products, not constructed options) → compare
  on spec/price/availability → decide → **purchase** behind approval →
  **track fulfillment** (shipping, delays) → verify received-and-correct →
  returns/aftercare if not.
- **Iterates on**: fulfillment events and mismatches.
- **Exits when**: possession verified correct — not at purchase, and not at
  shipment.
- **State**: a purchase ledger with a post-commit tracking tail.
- **Verification**: the actual delivered item matches the spec.
- **Time behaviour**: dormant-awaiting across days (tracking), like
  `arrange` but with an aftercare tail `arrange` doesn't have.
- **Split rationale (2026-10-08, split-first)**: vs `arrange` — the loop
  keeps running after the commit until possession is verified; that exit
  criterion and trailing state are the difference. A booking is done when
  confirmed; an acquisition is done when received.
- **Examples**: "buy a replacement filter", "order more of my shampoo",
  "get a USB-C dock that works with both laptops".

---

**`execute`** — *complete a known procedure.*

- **Entered when**: an institutional/administrative obligation must be
  completed — forms, claims, renewals, applications, taxes, paperwork. The
  process is largely predetermined by an external body.
- **Skeleton**: identify the procedure → instantiate its checklist
  (required documents, deadlines, order) → gather inputs → work the steps →
  submit → confirm acceptance.
- **Iterates on**: unchecked boxes, rejections, correction requests.
- **Exits when**: submitted and accepted — or blocked awaiting the user or
  the institution.
- **State**: a procedure-template instance + document checklist + deadlines.
- **Verification**: the institution's acceptance/acknowledgement.
- **Time behaviour**: bursty with long waits (deadlines months out).
- **Rail**: the plan is a **template** (pre-compiled, deadline-driven), not
  an emergent one — distinguishing it from both `conversation` and
  `arrange`.
- **Examples**: "renew my passport", "file the insurance claim", "do my
  taxes", "submit the expense report".

---

**`make`** — *produce an artifact.*

- **Entered when**: the deliverable is a thing — document, deck, code,
  plan, writing, a designed object.
- **Skeleton**: brief → draft → **check against a standard** (tests, review,
  the user's taste, explicit constraints) → revise → deliver.
- **Iterates on**: check failures and feedback.
- **Exits when**: the check passes / the user accepts.
- **State**: draft + check results.
- **Verification**: **a check result, never the model's claim** — tests
  green, or the user approved. (This is the shape that makes
  `software_development` an instance: `make` with tests as the checker.)
- **Time behaviour**: one burst (multi-iteration), occasionally long.
- **Examples**: "write the announcement", "build this feature", "make me a
  workout plan" (the artifact; *following* it is `tend`).

---

**`tend`** — *keep an ongoing part of life in good shape.*

- **Entered when**: a long-lived concern needs periodic care — money,
  health, wardrobe, home, relationships. Usually the n-th interaction with a
  standing concern, often arriving via a wake.
- **Skeleton**: review current state against goals → detect drift or
  opportunity → small interventions → record → **schedule the next review**.
- **Iterates on**: the schedule (wakes) and detected drift.
- **Exits when**: the next review is scheduled — never "done".
- **State**: per-domain history, goals, open threads — the durable,
  cross-session part.
- **Verification**: the user's periodic read of the history.
- **Time behaviour**: **long-horizon and scheduled** — this is the shape the
  wake/watch machinery ([`proactivity.md`](proactivity.md)) exists to serve.
- **Examples**: "how are we doing on the food budget this month", "does
  this jacket fit my wardrobe", "check on my investments quarterly".

---

**`learn`** — *build a skill over time.*

- **Entered when**: the user wants to acquire or improve an ability —
  language, instrument, sport, chess, a craft. Split from `tend`
  2026-10-08 (split-first): a curriculum is not drift-detection.
- **Skeleton**: assess current level → agree a target/curriculum → teach in
  sessions → exercise with feedback → **schedule spaced review** → track
  demonstrated progress across sessions.
- **Iterates on**: the review schedule (wakes) and the learner's
  demonstrated ability between sessions.
- **Exits when**: a session's goal is met and the next session/review is
  scheduled — never "done" globally.
- **State**: a skill progress model — level, curriculum position, what's
  shaky, the spaced-review queue.
- **Verification**: **demonstrated performance** (an exercise the learner
  actually did and got right), never the model's sense that progress
  happened.
- **Time behaviour**: long-horizon and scheduled, like `tend`.
- **Examples**: "teach me Spanish", "help me get better at chess",
  "practice guitar with me".

---

**`companion`** — *think and feel through something personal.*

- **Entered when**: the user wants reflection, emotional support, or
  counsel — nothing to be produced or committed. Includes `therapy`.
- **Skeleton**: listen → reflect → move **one** question at a time →
  maintain private notes → follow the user's lead; carries across sessions.
- **Iterates on**: the user's turns.
- **Exits when**: the user leads away. **No completion criterion exists.**
- **State**: private cross-session notes (themes, what matters, what moved).
- **Verification**: none — the model must never manufacture an exit.
- **Rails**: no tools by default, question-first, no completion pressure,
  privacy treatment distinct from task sessions.
- **Examples**: therapy conversations, "I'm stressed about the move",
  journaling prompts.

---

**`react`** — *handle something urgent.*

- **Entered when**: something **urgent or deteriorating** needs handling
  now — health, travel disruption, outage, safety, a deadline that has
  become imminent. Time pressure is the entry signal.
- **Skeleton**: triage severity/urgency → immediate stabilizing action
  (possibly under partial information) → confirm stable → reconcile and
  root-cause (often handing off to `diagnose`/`tend`) → schedule follow-up.
- **Iterates on**: the unfolding situation.
- **Exits when**: stabilized and follow-up scheduled.
- **State**: an incident log.
- **Verification**: a real-world check the situation has stopped worsening.
- **Rails**: **inverts the normal order — act before full understanding**;
  tight budget; escalation to the user is expected, not a failure.
- **Resolved (2026-10-08, split-first)**: stays a **label**, not a modifier
  on other shapes. The act-before-understanding ordering, the stabilization
  exit, and the incident-log state are exactly the state/exit differences
  the split-first rule says earn a label. Urgency that co-occurs with
  another shape is handled as a **handoff** (`react` → `arrange`/`diagnose`/
  `make`), which the registry needs anyway — one label plus handoffs beats
  two-axis classification plus composition. Entry requires **consequence
  for delay** (health, travel, safety, time-critical); mere impatience or
  "quickly" stays `conversation`.
- **Examples**: "my flight got cancelled", "the bathroom is flooding", "I
  think I lost my wallet abroad".

---

**`monitor`** — *watch a condition; decide; maybe speak.* (existing; not
message-classified)

- Runs as a wake/watch: check a condition → decide whether it matters now →
  stay silent or notify. The event engine that feeds `tend` and `react`.
- Already built ([`proactivity.md`](proactivity.md),
  [`event-delivery.md`](event-delivery.md)); listed here for completeness of
  the shape registry, not as a classification target for user messages.

### Entry criteria (operational — the labeling procedure)

How a request is labeled — by the classifier, by a human building the eval
set, or by the corpus generator. **First match wins**, judging only the
request text plus prior context:

| # | Test (first match wins) | Label |
|---|---|---|
| 1 | Urgent/deteriorating situation where delay has real consequences (safety, health, money, a missed critical window). | `react` |
| 2 | The outcome is **possession of a purchasable good** (buy / order / restock / replace). | `acquire` |
| 3 | The outcome is a **commitment in the world** — with other parties or the calendar (book, reserve, schedule, hire, plan-to-commit). | `arrange` |
| 4 | The request names a **known, externally-defined procedure** to be completed to submission/acceptance (renew, file, claim, apply, submit). | `execute` |
| 5 | The outcome is **an artifact the assistant produces**, judged against a standard (tests, a spec, taste). | `make` |
| 6 | The outcome is **the cause** of a specific malfunction or anomaly. | `diagnose` |
| 7 | The outcome is **understanding** assembled from investigation (multi-source, current, or synthesis-shaped). | `research` |
| 8 | The outcome is **the user's own ability**, built over sessions (practice, tutoring, review). | `learn` |
| 9 | The request concerns the **ongoing state of a standing concern** (review, maintain, adjust — recurring or wake-arrived). | `tend` |
| 10 | **Inner-life support with no outcome in the world** (explicitly reflective or support-seeking). | `companion` |
| 11 | None of the above — questions, quick asks, lookups, chatter, micro-tasks. | `conversation` |

**Global rules**

- **Judge the expected outcome, never the topic.** "Food" alone appears in
  `conversation` (a spend fact), `tend` (is the food budget on track),
  `diagnose` (why did food spend double), `acquire` (buy groceries),
  `make` (a meal plan).
- **Judge as written, from text + prior context only** — not what might turn
  out to be needed mid-way; handoffs cover that.
- **Task dominance over frame.** If any task shape applies, it wins over
  `companion` even when the message carries feeling: "I'm annoyed today —
  anyway, book the table" is `arrange`.
- **`react` and `companion` take conservative tests.** `react` needs
  consequence-for-delay, not mere "quickly". `companion` needs an explicit
  reflective/support-seeking frame: "I hate my job" is `conversation`;
  "I don't know what to do about my job" is `companion`. High precision
  matters — mislabeled-into-`companion` changes privacy handling.
- **Continuations resolve first.** "Continue", "do it", "yes please" are
  labeled by resolving them against the prior context into the request they
  stand for, then applying the table. (This is why the classifier's state
  must always carry the prior request(s) — the measured zero-shot failure
  mode.)
- **Compound requests: label the primary** — the shape of the outcome the
  turn will be judged by; a real-world commit or urgency dominates. The
  rest fan out as subagents ("find a dentist and research Invisalign" →
  `arrange`, research as a sub-loop). Ties → `conversation`.
- **Genuine ambiguity → `conversation`.** The fallback is safe by design;
  the dataset keeps it full of honest, varied fallback cases, not padding.

**Per-shape tests (long form)**

- **`react`** — *Yes*: present/imminent situation + real cost to delay.
  *No*: "remind me to renew my passport in June" (future, planned →
  `execute`-adjacent micro-task → `conversation`); worry about tomorrow
  (no consequence for delay → `companion`). *Boundary*: it only starts the
  turn — the actual fixing hands off (`react` → `arrange`).
- **`acquire`** — *Yes*: a good the user will possess. *No*: a lease,
  subscription, or service (a commitment → `arrange`); "make me a shelf"
  (create → `make`). *Boundary vs `arrange`*: possession of a thing vs a
  scheduled/committed arrangement. Implied purchases count ("we're out of
  detergent").
- **`arrange`** — *Yes*: the ask includes the commitment or coordination.
  *No*: "is Bali worth visiting in December?" (understanding → `research`);
  "where should I go?" (advice → `research`/`conversation`). *Boundary*:
  understanding now vs committing now decides `research` against
  `arrange`; "find me a dentist" implies contact/booking → `arrange`.
- **`execute`** — *Yes*: the request names the procedure itself ("renew my
  passport", "file the claim"). *No*: "what does renewing require?"
  (information → `conversation`/`research`); "book my passport appointment"
  framed as a reservation alone (→ `arrange`; when framed as part of the
  renewal → `execute`). *Boundary*: performing the procedure vs asking
  about it.
- **`make`** — *Yes*: a produced artifact with a standard it must meet
  (runs, compiles, lands with an audience, matches taste). *No*: "make me
  laugh" (no standard → `conversation`). *Boundary vs `research`*:
  investigating-and-writing-down → `research`; crafting-to-spec → `make`.
  "Write a cover letter" is `make`; "summarize what the literature says"
  is `research` — the difference is whether the findings or the craft are
  the point.
- **`diagnose`** — *Yes*: cause-seeking about a specific malfunction,
  anomaly, or failure. *No*: "how does X work" (→ `research`/`conversation`);
  "change X to do Y" with no malfunction (→ `make`/`execute`). *Boundary*:
  restore-broken → `diagnose`; produce-new → `make`. Includes causes in the
  user's own data ("why did my food spend double") and their own state
  ("why am I so tired" — with health-guardrail framing).
- **`research`** — *Yes*: the ask demands investigation — multi-source,
  current facts, comparison, or an explicit "look into / find out / work it
  out". *No*: a quick factual lookup (→ `conversation`, even when it uses a
  tool). *Boundary vs `learn`*: knowledge about the world → `research`;
  the user's ability over time → `learn`. "Explain how TLS works" =
  `conversation` (single reply), "teach me TLS in depth" = `research`,
  "teach me guitar" = `learn`.
- **`learn`** — *Yes*: the user will practice; outcome is ability. *No*:
  "explain X" (understanding once → `research`/`conversation`); "get me
  stronger" framed as health upkeep (→ `tend`; performance skills like
  chess/languages → `learn`).
- **`tend`** — *Yes*: assessment against goals or standing intent about a
  domain that persists ("how are we doing on X", "review my X", "does this
  fit my X"); wake-arrived standing instructions usually land here. *No*:
  a fact about the domain ("how much did I spend on food" →
  `conversation`). *Boundary*: fact vs review.
- **`companion`** — *Yes*: explicit reflective frame + no task. *No*: a
  bare feeling-report (`conversation`); a personal decision framed as
  analysis ("what are the tradeoffs of quitting" → `research`); anything
  with a task in it (task dominance).
- **`conversation`** — everything that falls through: questions, quick
  asks, lookups, chatter, reminders and other micro-tasks. Not a
  junk drawer — the majority class, and the safe destination.

**Boundary drills** (starter set; these double as the eval seed — hard
cases marked ⚠️):

| Request | Label | Why |
|---|---|---|
| Why is my gateway restarting? | `diagnose` | Cause of a specific malfunction. |
| How does our gateway routing work? | `conversation` | Understanding of a known system, no malfunction ("in depth" → `research`). ⚠️ |
| Book me a table for four, Saturday 8pm. | `arrange` | Commitment with a third party. |
| We're out of laundry detergent. | `acquire` | Implied purchase — label by the obviously-wanted outcome. ⚠️ |
| Plan the Bali trip for December. | `arrange` | Coordination-to-commit. |
| Is Bali worth visiting in December? | `research` | Understanding; no commit asked. |
| Renew my passport. | `execute` | Names the procedure, runs to acceptance. |
| What does renewing a passport require? | `conversation` | Information about the procedure, not performing it. ⚠️ |
| Write a cover letter for the Stripe job. | `make` | Artifact against audience/taste. |
| Research whether I should switch to a MacBook. | `research` | Decision support via investigation. ⚠️ |
| My flight just got cancelled and I fly tomorrow. | `react` | Consequence for delay; hands off to `arrange`. |
| Remind me to renew my passport in June. | `conversation` | Micro-task (a reminder), not the procedure. ⚠️ |
| Teach me Spanish. | `learn` | The user's ability, over sessions. |
| Explain how TLS works. | `conversation` | Single-reply understanding. ⚠️ |
| Review how my budget is doing this month. | `tend` | Assessment of a standing concern. |
| How much did I spend on food last month? | `conversation` | A fact. ⚠️ (Pairs with the row above.) |
| I'm feeling really anxious about this move. | `companion` | Reflective frame, no task. |
| I'm annoyed today — anyway, book the table. | `arrange` | Task dominance over frame. |
| Build me a spending dashboard. | `make` | Artifact with a standard (it works). |
| Why have I been so tired lately? | `diagnose` | Cause-seeking about own state (health guardrails apply). ⚠️ |
| Compare the top three EV lease deals. | `research` | Synthesis, no commit. |
| Sign me up for the best EV lease deal you find. | `arrange` | A lease is a commitment, not possession of a good. ⚠️ |
| Make me a workout plan. | `make` | Artifact. ("Do weekly workouts with me" → `tend`.) ⚠️ |
| Do it. (after a proposed booking) | `arrange` | Continuation resolves first, then labels. |

### Worked mappings

Domain-shaped asks from earlier discussion, resolved to shapes — the point
of the table is that *topics don't get labels*:

| The ask (topic) | Shape |
|---|---|
| "system debugging" of the user's infra | `diagnose` |
| "deep research" on a topic | `research` |
| "travel planning" (plan + book) | `arrange` |
| "buy a replacement filter" | `acquire` |
| "therapy" | `companion` |
| "teach me Spanish" | `learn` |
| "software development" | `make` (tests as the checker) |
| "how much did I spend on food" | `conversation` (+ finance tools) |
| "renew my passport" | `execute` |
| "review my budget every month" | `tend` (+ `monitor` wakes) |
| "flight cancelled, fix it" | `react` → `arrange` |

### Relationship to the existing architecture

- `conversation` **is** today's `TurnWorkflow` reason-act loop; nothing
  changes for it. `monitor` is the existing wake/watch path.
- Every other label, when it is eventually built, should be a **loop
  profile**: prompt sections + a pinned scratchpad template (the ledgers),
  a tool-provisioning set, budget/iteration defaults, and at most one or two
  deterministic rails (e.g. `diagnose`'s evidence-per-iteration rule;
  `make`'s check-result-as-done) carried on the *same* `TurnWorkflow`. A
  profile is a configuration of one loop, never a forked workflow family.
- **Classification channel**: one `choice` question at turn start over these
  labels, with the request plus a little prior context as the state; low
  confidence or `other`-equivalent resolves to `conversation`. A local,
  fine-tuned decision model (laya) is the working candidate — the label
  set defined here is the training target. Local experiment and measured
  baselines: `~/Documents/laya-experiment/` (zero-shot is decisively
  correct on clear-cut requests, abstains on continuations — both fixable
  with training data; not yet wired anywhere).

### Open questions

- **Compound requests** — *agreed 2026-10-08*: the classifier picks the
  *primary* shape; the loop may fan out subagents in other shapes. The
  primary-shape judgment itself is a data-quality risk to watch.
- **Open-set growth**: shapes are near-static, but new ones can emerge
  (e.g. `negotiate`?). Growth mechanism: sample the mis-routed tail
  periodically; a shape is added only with a crisp skeleton that differs on
  the axis table, never as a topic bucket.
- **Dataset feasibility**: training data must be generated against these
  entry criteria (per-shape synthetic corpus + held-out real turns as eval);
  boundary sharpness between shapes is the main data-quality risk, and the
  split-first rule makes boundaries *more* numerous even as it makes each
  one sharper.

### Notes Log

- 2026-10-08: **Introduced.** Derived in session while evaluating laya as a
  request classifier: the first taxonomy attempt (topic domains) and the
  second (workflow-shape verbs) were both rejected, the former by the owner
  ("I want each label to be defined based on its demand for a different
  shape of agentic loop"), the latter as developer-biased relative to a
  personal assistant's radius. This doc is the third, converging derivation.
  Also in-session: zero-shot laya baselines measured on real tenant turns
  (`~/Documents/laya-experiment/report.md`) — a fine-tuned model on this
  label set is the planned classifier; nothing wired.
- 2026-10-08 (later, same day): **Converged with the owner.** Four
  decisions: (1) `diagnose`/`research` stay split; (2) `learn` becomes its
  own shape, split from `tend`; (3) `react` stays a label rather than an
  orthogonal urgency flag — decided by the owner's own general rule, given
  as "even if the difference is in state or exit conditions, prefer
  splitting in general"; (4) compound requests = primary shape +
  subagent fan-out. That general rule is adopted as the doc's **split-first**
  principle, and applying it surfaced one further split the owner had not
  been shown: `acquire` out of `arrange` (an acquisition's loop keeps
  running past the commit until possession is verified — a different exit
  criterion plus a trailing tracking state). Set is now eleven labels plus
  `monitor`.
- 2026-10-08 (later, same day): **Entry criteria drafted** at the owner's
  direction — first-match labeling procedure (react → acquire → arrange →
  execute → make → diagnose → research → learn → tend → companion →
  conversation), global rules (outcome-not-topic; task dominance over
  frame; conservative `react`/`companion`; continuations resolve first;
  compound = primary + fan-out; ambiguity → `conversation`), per-shape
  long-form tests with near-misses, and 24 boundary drills (hard cases
  flagged) that double as the eval seed for the classifier. Next: corpus
  generation against this procedure.
