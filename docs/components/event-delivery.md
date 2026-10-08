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
it, and POSTs it to the harness; the harness turns it into a wake; the agent
decides what, if anything, to say. The engine never talks to Temporal, and the
harness never learns what a budget is.

```
engine write  ──▶  detect (same transaction)  ──▶  record (own table)
                                                        │
                                          delivery ─────┘
                                                        ▼
                       POST <gateway>/events  { token, event }
                                                        │
                              verify HMAC → session key ─┤
                                                        ▼
                            SignalWithStart Coordinator (Wake)
                                                        │
                                                    agent turn
```

### Why in-cluster, not through mcp-hub

`proactivity.md` said mcp-hub owns the public webhook hop. That is still right for
**external providers pushing at you** (a calendar webhook), and it stays deferred.
It is *not* needed for engine-raised events: engines and the gateway sit in the
same tenant namespace, so an engine POSTs to
`http://gateway.<ns>.svc.cluster.local:8090/events` and nothing is exposed
publicly at all. Fewer moving parts, no unauthenticated ingress, no mcp-hub
change. mcp-hub becomes relevant only when a third party needs to reach us.

### The callback token

`base64(session_key | expiry) + HMAC-SHA256`, secret from the environment.

- The engine stores it **verbatim and opaquely** — it never parses or trusts it,
  only echoes it back. `finance.budgets.callback_token` already does this.
- The gateway verifies it and reads the session key out, so **the harness needs no
  mapping table** and the engine never learns what a session is.
- Revocation needs no revocable token: cancelling deletes the engine's row, so no
  further callbacks arrive.
- Minting lives in `tenant-worker`'s `arm_wake` (Python, event path); verification
  lives in the gateway (Go). Cross-language, hand-mirrored — the same tolerated
  duplication as `types.go`/`types.py`. The format must be documented in one place
  and asserted by tests on both sides.

### Delivery, concretely

1. **A delivery worker per engine**, mirroring `poller.py`'s shape in mcp-hub:
   one injectable `httpx.AsyncClient`, an `asyncio` task started next to the
   existing loops, cancelled in the same `finally`. No new deployment — it lives
   inside the API process.
2. **Claim before send.** Take the oldest undelivered row, set `delivered_at` on
   success. On failure leave it and retry with backoff — mcp-hub's
   `min(interval * 2**failures, 900)` is the precedent.
3. **Dedupe by event id.** The harness must not wake twice for a retried POST, so
   the request carries an event id and the ingress rejects a repeat. Ordering
   matters here: mark delivered only after the harness has accepted it, or a crash
   between the two loses the event silently.
4. **Never log bodies.** Provider and engine payloads can carry credentials;
   mcp-hub's own rule, and it applies here.

Gateway side: `/events` on the existing mux, HMAC verify, `SignalWithStart` the
coordinator with a `WakePayload` whose `wake_id` is the engine's row id, so
`turns.initiated_by` reads `wake:<budget-or-arrival-id>`.

### Use case 1 — finance budgets (detection built)

Already done in `finance-engine` `b6eeb22`: a spend is written, and the same
transaction asks whether it pushed any budget past its alert line, recording the
crossing once. `budget_fires.delivered_at` is the column the delivery worker
drains; `callback_token` is already stored.

**Remaining:** the delivery worker, and the `/events` ingress. Nothing else — the
detection, the dedup and the record all exist.

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

- `arrival_briefing_set(enabled, callback_token)` — turn briefings on or off
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

1. **`/events` ingress + token mint/verify.** Everything else is blocked on it,
   and it is testable on its own: a valid token wakes the right session, a
   tampered one does not, a replayed event id does not wake twice, an expired one
   is ignored.
2. **finance delivery worker.** Smallest possible proof of the whole path, because
   detection is already done and a crossing can be produced by writing a spend.
3. **maps arrival** — the `trips_api.go` hook, the preference, the delivery worker
   (a copy of finance's).
4. **Android budgets mirror** — models, fixtures and operation shapes all exist, so
   this is a mirror rather than a design job. Held for now.
5. **Home strip** — `BudgetsModel.alerted` is already the hook; a small surface,
   not a screen.

### Open questions

- **Token expiry.** The signed token carries one, but a budget or a briefing can
  outlive it. Either the engine refreshes on a `401` from the ingress, or the
  token's expiry is set far enough out to outlive the row. Leaning the latter
  with a re-mint on edit.
- **What an arrival briefing costs.** One wake per drive end could be a lot of
  messages. The agent decides whether to speak, and `recall` holds "stop doing
  this" corrections, but a per-day cap may be needed and is not designed.
- **Batching.** Several crossings in one evening currently mean several wakes. The
  coordinator serializes them and each turn sees what the last one said, so they
  fold rather than pile up — but a digest is not designed.
