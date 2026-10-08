# Component: Event delivery — engines that notice, and the wake they cause

> STATUS: PLAN (2026-10-08). Follows the wake work in
> [`proactivity.md`](proactivity.md), which removed `IntentionWorkflow` and
> settled that a wake is a Temporal Schedule. This doc covers the *other* half:
> events raised by an engine rather than by a clock.
>
> Built: the finance side's detection (`finance-engine` `b6eeb22`) and the whole
> mcp-hub half of the delivery path — `/events`, the `subscriptions` table, and the
> wake itself (`infra` `810ec9e`). Remaining: the per-engine delivery workers, the
> maps arrival hook, and wiring `arm_wake(on_event=...)` so that anything actually
> arms a subscription.

### The shape, in one line

An engine notices something about its own data **when the data lands**, records
it, and POSTs it to mcp-hub; mcp-hub invokes the id that event is registered
against; the agent decides what, if anything, to say. The engine never talks to
Temporal and never learns a session key; mcp-hub never learns what a budget is,
nor what the id it invokes means.

```
engine write  ──▶  detect (same transaction)  ──▶  record (own table)
                                                        │
                                          delivery ─────┘
                                                        ▼
                    POST <mcp-hub>/events  { event_key, objective, event_id }
                                                        │
                              subscriptions: event_key → invoke_id  (opaque)
                                                        ▼
                    SignalWithStart invoke_id
                              (workflow type + signal name from config)
                                                        │
                                                    agent turn
```

### mcp-hub owns the routing

**Corrected 2026-10-08.** This section previously argued for an in-cluster POST
straight to the gateway. That was wrong on two counts. mcp-hub owns and manages
the external engines, so the harness must not hold a direct line to them — and an
engine must not need to know the harness exists at all. Engines are connections;
mcp-hub is the boundary in both directions.

The engine therefore emits **domain facts only**:

```
POST <mcp-hub>/events
{ "event_key": "budget:<id>|2026-10", "objective": "...", "event_id": "<dedupe>" }
```

No session, no callback URL, no credential, no awareness that anything is
listening. mcp-hub holds `event_key -> invoke_id` in its own `subscriptions` table,
where `invoke_id` is **opaque** — minted by the arming side, stored and reused
verbatim, never parsed, resolved or branched on. Even the workflow type and signal
name are configuration rather than constants, so mcp-hub's source contains no
vocabulary for sessions, schedules or coordinators at all:

```
mcp-hub ──▶ Temporal   SignalWithStart(invoke_id, <configured signal>)
                                                        │
                                                    agent turn
```

SignalWithStart rather than Signal because what is invoked may not be running, and
an event arriving while it is stopped still has to land; if it *is* running, the
signal is delivered into it and whatever handles it continues from there — which is
what turns an event into a turn. One mechanism covers both, so "the session may or
may not be running" never has to be reasoned about at this layer.

**The harness mints the id at arm time.** `arm_wake(on_event=...)` decides what a
wake means — today, the coordinator's own workflow id — and hands mcp-hub that id
and nothing else. The coupling runs one way: mcp-hub knows there is *an id*; the
harness decides what it is, and can change that decision without touching mcp-hub.

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

1. **A delivery worker per engine**, mirroring `poller.py`'s shape in mcp-hub:
   one injectable `httpx.AsyncClient`, an `asyncio` task started next to the
   existing loops, cancelled in the same `finally`. No new deployment — it lives
   inside the API process.
2. **Claim before send.** Take the oldest undelivered row, set `delivered_at` on
   success. On failure leave it and retry with backoff — mcp-hub's
   `min(interval * 2**failures, 900)` is the precedent.
3. **Dedupe by event id, enforced in Temporal.** A retried POST carries the same
   event id, so it computes the same workflow id: `REJECT_DUPLICATE` once the wake
   has completed, `USE_EXISTING` while one runs. The delivery worker needs no dedupe
   table of its own — but it still marks `delivered_at` only after mcp-hub has
   accepted the POST, or a crash between the two loses the event silently.
4. **Never log bodies.** Provider and engine payloads can carry credentials;
   mcp-hub's own rule, and it applies here.

mcp-hub side: a `subscriptions` table (`event_key -> invoke_id`, expiry-checked the
way `oauth_tokens` already is) plus a SignalWithStart against the tenant's Temporal
using the arming side's id. `turns.initiated_by`
reads `wake:<event_key>:<event_id>`.

### Use case 1 — finance budgets (detection built)

Already done in `finance-engine` `b6eeb22`: a spend is written, and the same
transaction asks whether it pushed any budget past its alert line, recording the
crossing once. `budget_fires.delivered_at` is the column the delivery worker
drains, and `budget:<id>|<period_key>` is its `event_key`.

**Remaining:** the delivery worker, and mcp-hub's side of the path. Nothing else —
the detection, the dedup and the record all exist.

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
insert an arrival row, and let the delivery worker drain it. The payload is small:
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

1. ~~**mcp-hub's `/events` route + `subscriptions` table.**~~ **DONE** (`infra`
   `810ec9e`; mcp-hub's 142 tests green). A known `event_key` invokes the id
   registered against it, an unknown one is a 404, a replayed event id returns 202
   `duplicate` having invoked nothing a second time, and an expired subscription is
   ignored — expiry decided in SQL and tested against a real Postgres, so the clock
   the test runs on cannot make a stale row look live. The id is opaque and the
   workflow type and signal name are configuration, so mcp-hub names no session, no
   schedule and no coordinator anywhere.
2. **finance delivery worker.** Smallest possible proof of the whole path, because
   detection is already done and a crossing can be produced by writing a spend.
3. **maps arrival** — the `trips_api.go` hook, the preference, the delivery worker
   (a copy of finance's).
4. **Android budgets mirror** — models, fixtures and operation shapes all exist, so
   this is a mirror rather than a design job. Held for now.
5. **Home strip** — `BudgetsModel.alerted` is already the hook; a small surface,
   not a screen.

### Open questions

- **Nothing subscribes yet.** No engine exposes a subscribe operation, and
  `finance` emits on every budget crossing whether or not anyone is listening — so
  today the toggle is an engine-side flag, not something the agent opens. Wiring
  `on_event` means: agent calls the provider's subscribe tool *after* mcp-hub holds
  the row (harness first, provider second), so an event cannot arrive before
  anything can route it.
- **What an arrival briefing costs.** One wake per drive end could be a lot of
  messages. The agent decides whether to speak, and `recall` holds "stop doing
  this" corrections, but a per-day cap may be needed and is not designed.
- **Batching.** Several crossings in one evening currently mean several wakes. The
  coordinator serializes them and each turn sees what the last one said, so they
  fold rather than pile up — but a digest is not designed.
