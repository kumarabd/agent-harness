# Component: Mobile Gateway

> STATUS: SLICE 1 built (text in/out, streaming, multi-device fan-out, cursor
> resume, `ask_user`). `cancel`/stop and cross-replica presence built
> 2026-09-12 (neither yet deployed/live-verified). Deferred: `status` frames
> tuning, tool-activity frames.

## Role

An authenticated WebSocket (`GET /ws` on the gateway process) that an iOS /
iPadOS app connects to. STT/TTS are out of scope — this is text only, realtime,
bidirectional.

- **Inbound** frames normalize into a `core.MessageEvent{Platform:"mobile"}` and
  go through the shared `core.Ingestor` (session resolve → dedup → `SignalWithStart`)
  — identical to web.
- **Outbound** is a live stream. The turn workflow writes to `messages` /
  `turn_deliveries` / `turn_status_pings` exactly as before; a `NOTIFY` trigger
  (`mobile_stream`, payload = session_key) wakes the gateway replica's hub; each
  WS connection re-reads *its own tail* and pushes frames.

## Session model — one per user, fanned out

Session key: `agent:main:mobile:user:<clerkUserID>` (mirrors web). Every device a
user has connects to the **same** session and tails the same turns — continue a
conversation across iPhone and iPad, and a proactive turn (`IntentionWorkflow` →
`Wake`) is delivered once and seen everywhere.

Concurrency: two devices sending within the same window → the coordinator's
active-turn guard folds the second in as an interrupt (cancels in-flight work,
re-reasons). Same as two people in a Discord channel.

Echo: a device shows its own message optimistically, then receives it back over
the fan-out. `messages.client_msg_id` (stamped by the sender, threaded through
the signal payload) is echoed in the `message` frame so the sender dedupes;
`speaker_id` (the device id) lets other devices render "from another device".

## The cursor is client-owned

The server keeps **no per-device read state**. The client persists its resume
point and sends it on (re)connect:

- **Cursor = `turn_seq`** (coarse — the highest fully-received turn). `turn_seq`
  is strictly monotonic per session.
- **Resume**: replay every turn after the cursor *in full*; for the in-progress
  turn, send a snapshot (`delta` with `replace:true`) of the current streamed
  content, then live deltas.
- **Cold client** (no cursor): the last `coldStartTurns` (20) turns.
- **Way behind**: capped at 20 turns of replay.

## NOTIFY is a wake hint, never a log

A missed notification (LISTEN connection blip) self-heals: a connection always
re-reads from its own cursor, never trusts a payload to be complete. On LISTEN
(re)connect the hub wakes every session it serves. The hub holds **one** LISTEN
connection per gateway replica, not one per socket.

## Streaming

`model_call.py` streams token chunks into `turn_deliveries` (cumulative content)
for `platform in (discord, discord-voice, mobile)` on the turn's first call.
For discord/voice it also signals the workflow (`MODEL_CALL_CHUNK_SIGNAL`, drained
by `turn.go` to dispatch a per-connection delivery activity). **Mobile does not
signal** — there's no connection to dispatch to; the gateway tails the table via
the NOTIFY trigger. `turn_deliveries.content` stays cumulative (Discord edits a
message in place); the gateway diffs it to emit deltas.

## Wire protocol

Client → server: `{type:"auth", token, device_id, after_turn_seq?}` (first frame,
required), `{type:"message", client_msg_id, text, device_id?}`,
`{type:"answer", request_id, selected_option_id|free_text}`,
`{type:"resume", after_turn_seq}`.

Server → client: `turn_start`, `message`, `delta` (`replace?`), `status`,
`ask_user`, `turn_end` (`status`), `resumed`, `error`. Keepalive is WS
ping/pong (25s).

## Auth

Clerk JWT in the first frame (not a header — some ingress strips non-standard
headers on WS upgrade). Verification is the shared `internal/gateway/clerkauth`
package (extracted from web). The Clerk `sub` is the user id; the session key is
derived from it, never trusted from the client.

## Files

`internal/gateway/mobile/`: `mobile.go` (Handler, `GET /ws`), `conn.go`
(per-connection: auth, read pump, tail/catchup), `hub.go` (per-replica LISTEN +
registry), `frames.go` (wire types), `tail.go` (pure cursor/diff helpers,
unit-tested in `tail_test.go`), `presence.go` (cross-replica presence — see
below). `internal/gateway/clerkauth/` (shared JWT verification). Migration
`032` (`messages.client_msg_id` + the NOTIFY triggers), `033`
(`mobile_presence`).

## Cancel / stop

`{"type":"cancel"}` (mobile) / `POST /cancel` (web) → `core.CancelActiveTurn`
(ownership-checked) → `CancelSignalName`, its own Temporal signal — never a
`NewMessage` payload, since several turn-loop branches treat "a message
arrived" as "fold it in and keep reasoning," the opposite of a stop. Ends the
turn immediately (no further `ModelCall`), `turns.status = "cancelled"` (a
value mobile/web's `turn_end`/`isTerminal()` already accepted before this was
built). Full detail: `docs/components/gateway/first-party-plan.md` §7.

## Cross-replica presence

A real table (`mobile_presence`, migration `033`), not a Temporal
signal/Query on `CoordinatorWorkflow` — the coordinator idles out and exits
with no active turn while a device stays connected via the NOTIFY tail
entirely independent of it, which is exactly the state a presence check most
needs to answer during. Presence is connection-layer, ephemeral,
non-deterministic state (the same category the NOTIFY mechanism itself is —
best-effort, self-healing), not durable business state Temporal workflows
model.

- **Heartbeat rides the existing WS ping** (`conn.go`'s `pingEvery`, 25s) — no
  new timer. Every read is staleness-filtered
  (`last_seen_at > now() - interval '90 seconds'`), never trusts row
  existence alone — a crashed connection (no clean disconnect) ages out on
  its own, no reaper needed.
- Connect → upsert; clean disconnect → delete (immediate, but not
  load-bearing — staleness is the real safety net).
- `presence.go`'s `Present(ctx, pool, sessionKey)` is the authoritative,
  cross-replica read. No cross-language RPC — a future consumer (e.g.
  proactivity's `CheckCondition`, Python) just queries `mobile_presence`
  directly, same as any other table in this system. Not yet wired to any
  consumer — the table + write path is built, nothing reads it yet.

## Deferred

- **`status` backoff for mobile** — `turn.go`'s `statusPingBackoff` has no
  `"mobile"` case yet, so the watchdog doesn't run for mobile. Decide whether
  the app infers progress from tool activity instead.
- **Tool-activity frames** — the app currently sees only messages + deltas +
  status, not "running search…".
