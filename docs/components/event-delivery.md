# Component: Event delivery — engines that notice, and the wake they cause

> STATUS: PLAN (2026-10-08). Follows the wake work in
> [`proactivity.md`](proactivity.md), which removed `IntentionWorkflow` and
> settled that a wake is a Temporal Schedule. This doc covers the *other* half:
> events raised by an engine rather than by a clock.
>
> Nothing here is built except the finance side's detection, which is committed
> (`finance-engine` `b6eeb22`). The delivery path is the shared blocker for
> everything else.

### The shape, in one line

An engine notices something about its own data **when the data lands**, records
it, and POSTs it to mcp-hub; mcp-hub turns it into a wake; the agent decides
what, if anything, to say. The engine never talks to Temporal and never learns a
session key; mcp-hub never learns what a budget is.

```
engine write  ──▶  detect (same transaction)  ──▶  record (own table)
                                                        │
                                          delivery ─────┘
                                                        ▼
                    POST <mcp-hub>/events  { event_key, objective, event_id }
                                                        │
                              subscriptions: event_key → session_key
                                                        ▼
                    start WakeWorkflow   id = wake:<event_key>:<event_id>
                                                        │
                                          WakeSession ──┤ (existing activity)
                                                        ▼
                            SignalWithStart Coordinator (Wake)
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
listening. mcp-hub holds `event_key -> session_key` in its own `subscriptions`
table and **signals the session itself** — no harness ingress exists at all:

```
mcp-hub ──▶ Temporal   start_workflow(WakeWorkflow, id="wake:<event_key>:<event_id>")
                             └─▶ WakeSession activity ─▶ SignalWithStart Coordinator (Wake)
```

mcp-hub starts the *same* `WakeWorkflow` a Temporal Schedule starts, rather than
replicating `WakeSessionActivity`'s SignalWithStart itself. One wake path, two
triggers, and the coordinator handoff stays in the one place that already gets it
right.

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

   **Dedupe is now Temporal's, not a table's.** The wake workflow id is derived
   from the event id, so a retried event cannot wake twice — `REJECT_DUPLICATE` on
   a completed id and `USE_EXISTING` on a running one are both no-ops. This is the
   one piece of the earlier plan that claimed Temporal could not absorb the work;
   it can, once the id is the event id.
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

mcp-hub side: a `subscriptions` table (`event_key -> session_key`, expiry-checked
the way `oauth_tokens` already is) plus `start_workflow` against the tenant's
Temporal, with the coordinator handoff left to `WakeWorkflow`. `turns.initiated_by`
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

1. **mcp-hub's `/events` route + `subscriptions` table.** Everything else is
   blocked on it, and it is testable on its own in mcp-hub: a known `event_key`
   wakes the right session, an unknown one is dropped, a replayed event id does not
   wake twice, an expired subscription is ignored.
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
