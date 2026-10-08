# Component: Event delivery — engines that notice, and the wake they cause

> STATUS: PLAN (2026-10-08). Follows the wake work in
> [`proactivity.md`](proactivity.md), which removed `IntentionWorkflow` and
> settled that a wake is a Temporal Schedule. This doc covers the *other* half:
> events raised by an engine rather than by a clock.
>
> Built end to end for finance: detection, the delivery contract, mcp-hub's
> per-connection delivery loop, and `arm_wake(on_event=...)` to arm a watch.
> Remaining: the maps arrival hook and its half of the contract, which is now a
> copy of finance's rather than a design job.

### The shape, in one line

An engine notices something about its own data **when the data lands** and records
it. It does not send it anywhere. mcp-hub asks each connection what it has noticed,
invokes whatever each notice is registered against, and tells the connection it
landed. So an engine never talks to Temporal, never learns a session key and never
holds mcp-hub's address; mcp-hub never learns what a budget is, nor what the id it
invokes means.

```
engine write ──▶ detect (same transaction) ──▶ record (own table)
                                                     ▲
                                    events_ack ──────┤  marks it delivered
                                                     │
     mcp-hub, one delivery loop per connection ──────┤
                                                     │  events_pending
                                                     ▼
                              subscriptions: event_key → invoke_id  (opaque)
                                                     │
                                                     ▼
                              SignalWithStart CoordinatorWorkflow
                                     id = invoke_id,  signal = Wake
                                                     │
                                                     ▼
                                       coordinator folds it in as a turn
```

### mcp-hub owns the routing

**Corrected 2026-10-08.** This section previously argued for an in-cluster POST
straight to the gateway. That was wrong on two counts. mcp-hub owns and manages
the external engines, so the harness must not hold a direct line to them — and an
engine must not need to know the harness exists at all. Engines are connections;
mcp-hub is the boundary in both directions.

**An engine's whole half is a pair of operations.** `events_pending` hands over what
it has recorded and not yet delivered; `events_ack` marks those delivered. Both are
ordinary operations on the engine's existing registry, so they arrive over MCP and
HTTP alongside everything else it exposes — no new transport, no endpoint, no
credential, no awareness that anything is listening:

```
events_pending  ->  { items: [ { event_key, event_id, objective, why } ] }
events_ack      <-  { event_ids: [ ... ] }
```

That pair is the *only* thing named for a mechanism rather than a use case in either
engine, and deliberately so: mcp-hub finds it by name on every connection and calls
it blind, which is what lets one delivery loop serve budget crossings and map
arrivals without knowing what either is.

mcp-hub holds `event_key -> invoke_id` in its own `subscriptions` table and signals
that id:

```
mcp-hub ──▶ Temporal   SignalWithStart(CoordinatorWorkflow, id=invoke_id,
                                       signal=Wake, payload=WakePayload)
                                                        │
                                    coordinator folds it in as a turn
```

**This is the ordinary turn flow, not a mechanism of its own.** The coordinator is
bound to one session and every turn reaches it as a Temporal signal — the gateway
signals a user message in on `NewMessage`, and a wake is the same call on `Wake`.
The only difference is who is speaking: with an event, it is the agent. The
coordinator handles the two as siblings, starting a proactive turn when nothing is
running (`initiated_by` reading `wake:<id>`) and folding the objective into the live
turn when one is.

**The arming side supplies the id.** `arm_wake(on_event=...)` decides what a wake
should reach and hands mcp-hub that id; mcp-hub never derives it and never
interprets it. Today the harness sets it to a session's coordinator — but that is
the harness's decision, not a fact mcp-hub holds, so it is stored as an opaque
`invoke_id` rather than named after what it happens to contain.

This is the shape the agent's `arm_wake` path registers, and the same idiom
mcp-hub already uses for cross-boundary routing — `oauth_tokens` stores opaque,
expiry-checked, `compare_digest`-compared tokens for exactly this reason, and a
subscription is another such row.

Three consequences worth stating:

1. **No callback token.** An earlier draft signed the session key into a token the
   engine stored. That is gone: the engine persists nothing it cannot use, and
   `finance.budgets.callback_token` goes away before it is ever read. The engine
   never holds a credential whose only purpose is to talk to us.
2. **There is no harness ingress, so there is nothing to secure.** The
   direct-to-gateway design was worse than it looked — the router proxies
   `/gateway/*` and strips the prefix, so `POST <router>/gateway/events` would
   have reached such a route **publicly and unauthenticated**, letting anyone wake
   any session with a chosen prompt. That entire class of problem is gone rather
   than mitigated: mcp-hub already sits inside the tenant and already holds the
   engines' credentials, so it needs no new trust relationship to do this.

   **Dedupe is a row here, not Temporal's.** Because the invoked id belongs to the
   arming side, it is shared by every event that target will ever receive — so
   Temporal cannot distinguish a retry from a new event. `event_deliveries` is the
   idempotency table instead, written only *after* an invoke succeeds, so a crash in
   between leaves the retry able to re-deliver. A missed alert is worse than a
   repeated one.
3. **The engine stays ignorant of sessions** — the original principle, and the
   thing the direct design quietly broke.

### Delivery, concretely

1. **One delivery loop per connection**, in mcp-hub's `run_forever` beside that
   connection's poll loop and created the same way — a connection whose wakes keep
   failing cannot stall the others, and registering a connection starts both. It
   runs on a shorter interval than indexing, because a notice is time-sensitive in a
   way a tool description is not.
2. **Ask, wake, ack, in that order.** `events_pending` returns what the engine has
   recorded; each notice wakes whatever its `event_key` is registered against; and
   only what actually woke is acked. A failed wake stays pending and the loop's own
   backoff (`min(interval * 2**failures, 900)`) brings it back.
3. **A notice nobody subscribed to is acked too.** It can never become deliverable,
   and leaving it pending would hand it back on every tick for the life of the
   connection.
4. **`event_deliveries` is the safety net, not the mechanism.** The engine's ack is
   what normally stops a repeat; the table covers the window where a wake succeeded
   and the ack did not, which would otherwise wake the same session twice.
5. **Never log bodies.** Provider and engine payloads can carry credentials;
   mcp-hub's own rule, and it applies here.

mcp-hub side: a `subscriptions` table (`event_key -> invoke_id`, expiry-checked the
way `oauth_tokens` already is) plus the same SignalWithStart the gateway makes, on
the `Wake` channel. `turns.initiated_by` reads `wake:<event_key>:<event_id>`.

### Use case 1 — finance budgets (detection built)

Already done in `finance-engine` `b6eeb22`: a spend is written, and the same
transaction asks whether it pushed any budget past its alert line, recording the
crossing once. `finance-engine` `1dee400` adds the `events_pending`/`events_ack` pair
that hands those crossings over and marks them delivered, keyed by `budget:<id>` —
deliberately **not** scoped by period. A subscription is a standing intent ("tell me
when Dining out goes over"), so a key carrying the period would stop matching the
moment the calendar moved on, silently, a month after anyone could have noticed.
Once-per-period is already the fire's own guarantee: its id is unique per crossing,
and that is the `event_id`.

**Remaining:** nothing. The detection, the record, the contract and the delivery loop
all exist; what is missing is anything that *arms* a subscription, below.

`finance.budgets.callback_token` is **dead** and should be dropped in the same
change that adds the worker. It was minted for the deleted signed-callback design
and nothing has ever read it.

### Use case 2 — maps arrival briefing

**The event is a drive ending.** Much better than a configured geofence: the
arrival already exists in the data model, so there is no radius or point to
configure, no setup UI, and no boundary dataset to obtain. Place resolution moves
*after* the fact, where the agent can do it and judge whether it is worth doing —
instead of a Places call at detection time on every drive.

**The engine's whole job** is at the one place a trip completes:

`internal/api/trips_api.go:85` — `repo.UpdateTripStatus(r.Context(), id, "completed", false, true)`

On that path: read the trip's last telemetry sample for its coordinate (one
query — `trips` has no location columns, the trace is in `telemetry_samples`),
insert an arrival row, and let mcp-hub's delivery loop collect it. The payload is small:
trip id, final lat/lng, completed_at, and the vehicle. **No reverse geocoding in
the engine** — that is the agent's job, and doing it here would put a paid API call
on every drive.

**The agent's job**, on the wake: call `reverse_geocode` — *already an MCP tool in
`maps-engine`* (`internal/mcpserver/geocode.go`, listed in the integration test's
tool set) — and write a message about where the user is. That is the product: the
agent says something about the place, not "you arrived at 12.9716, 77.5946".

**The only new surface** is a preference:

- `arrival_briefing_set(enabled)` — turn briefings on or off
- `arrival_briefing_get()`

Named for the use case, per the rule the finance work settled: a tool the agent
picks by should describe the outcome, not the mechanism. A single toggle rather
than a watch table, because there is nothing per-arrival to configure. If it later
needs to be per-vehicle, that is a vehicle column, not a new concept.

Deliberately **not** doing: `watch_arrival` / `watch_departure` geofences. They
remain a real feature for "tell me when I get home", but they need a point, a
radius and a setup flow, and the drive-ending event delivers the user's stated ask
without any of that.

### Ordering

1. ~~**mcp-hub's delivery loop + `subscriptions` table.**~~ **DONE** (`infra`
   `4aa6155`; mcp-hub's 140 tests green). A notice wakes whatever its `event_key` is
   registered against; one nobody subscribed to is acked and dropped; an event
   already delivered is acked without waking again; and an expired subscription is
   ignored — expiry decided in SQL and tested against a real Postgres, so the clock
   the test runs on cannot make a stale row look live.

   **Waking needs `ALLOW_DUPLICATE`.** The two Temporal policies cover disjoint
   cases — `id_conflict_policy` while an execution is running, `id_reuse_policy`
   once it has closed — and the coordinator exits *cleanly* on idle TTL. Temporal's
   default reuse policy allows an id back only if the previous run failed, so
   without `ALLOW_DUPLICATE` the ordinary case, waking a session that has gone
   idle, is refused. `cmd/starter/main.go` already set it for the chat path;
   `WakeSession` did not, and now does (agent-harness `0fdd504`).
2. **finance's half of the contract.** Smallest possible proof of the whole path, because
   detection is already done and a crossing can be produced by writing a spend.
3. **maps arrival** — the `trips_api.go` hook, the preference, and the same
   `events_pending`/`events_ack` pair finance now implements, publishing its own
   `event_key` the way `budgetEventKey` does. mcp-hub needs no change at all: its
   delivery loop finds the pair by name on any connection, and the harness already
   arms a watch against whatever key an engine publishes.
4. **Android budgets mirror** — models, fixtures and operation shapes all exist, so
   this is a mirror rather than a design job. Held for now.
5. **Home strip** — `BudgetsModel.alerted` is already the hook; a small surface,
   not a screen.

### Open questions

- **How does the agent learn an `event_key`?** It reads one. `budget_status` and
  `budgets_list` now return `event_key` alongside each budget, so the agent passes
  what the engine publishes instead of assembling a key from an id and a guessed
  format — and both come from one definition (`budgetEventKey`), so the key published
  can never drift from the key raised. A future engine has to do the same, and that
  is the real cost of the contract: not the two operations, but the obligation to
  publish the keys they will raise.
- **A watch cannot be revised.** It has no cadence, and its objective comes from the
  engine afresh each time, so watching something else is a different `event_key`
  rather than an edit. `revise` says exactly that instead of pretending. If that
  proves annoying, the fix is a stored event_key the harness can update — which is
  one column, not a redesign.
- **What an arrival briefing costs.** One wake per drive end could be a lot of
  messages. The agent decides whether to speak, and `recall` holds "stop doing
  this" corrections, but a per-day cap may be needed and is not designed.
- **Batching.** Several crossings in one evening currently mean several wakes. The
  coordinator serializes them and each turn sees what the last one said, so they
  fold rather than pile up — but a digest is not designed.
