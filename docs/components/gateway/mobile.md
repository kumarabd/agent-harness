# Component: Mobile Gateway

> STATUS: SLICE 1 built (text in/out, streaming, multi-device fan-out, cursor
> resume, `ask_user`). `cancel`/stop, cross-replica presence, `KeepAlive`
> (coordinator lifetime tied to connection liveness), device_id/speaker_id
> separation, and status pings (as `delta`, not a frame type) — ALL BUILT +
> DEPLOYED + LIVE-VERIFIED 2026-09-12. Voice/text mode built same day (NOT
> yet deployed/verified). Deferred: tool-activity frames, wiring a
> `Present()` consumer, session list/select.

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
`messages.client_device_id` (migration `034`) lets other devices render "from
another device". **Corrected 2026-09-12**: this used to be `speaker_id`
holding the device id — wrong, since `speaker_id` is supposed to be the human
(the same across every one of a user's devices), per
`first-party-plan.md` §2. `speaker_id` now always carries the verified Clerk
sub; device attribution is its own column and its own `DeviceID` field on the
wire (`messageFrame`), never conflated with identity again.

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
required), `{type:"message", client_msg_id, text, device_id?, mode?}`,
`{type:"answer", request_id, selected_option_id|free_text}`,
`{type:"resume", after_turn_seq}`.

Server → client: `turn_start`, `message`, `delta` (`replace?`), `ask_user`,
`turn_end` (`status`), `resumed`, `error`. Keepalive is WS ping/pong (25s).
No separate `status` frame — a progress ping arrives as a `delta` (see
"Status pings" below).

## Auth

Clerk JWT in the first frame (not a header — some ingress strips non-standard
headers on WS upgrade). Verification is the shared `internal/gateway/clerkauth`
package (extracted from web). The Clerk `sub` is the user id; the session key is
derived from it, never trusted from the client.

## Files

`internal/gateway/mobile/`: `mobile.go` (Handler, `GET /ws`), `conn.go`
(per-connection: auth, read pump, tail/catchup, presence + `KeepAlive`
tickers), `hub.go` (per-replica LISTEN + registry), `frames.go` (wire types),
`tail.go` (pure cursor/diff helpers, unit-tested in `tail_test.go`),
`presence.go` (cross-replica presence table — see below).
`internal/gateway/core/`: `access.go` (`CancelActiveTurn`, ownership checks),
`inbound.go`'s `Ingestor.KeepAlive`. `internal/gateway/clerkauth/` (shared JWT
verification). Migration `032` (`messages.client_msg_id` + the NOTIFY
triggers), `033` (`mobile_presence`). `internal/workflow/{coordinator,turn}.go`
carry `CancelSignalName` and `KeepAliveSignalName` — see each's own doc
comment.

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

### `KeepAlive` — a related but distinct mechanism

The table answers "is a device connected." A separate question — "should the
session's `CoordinatorWorkflow` itself stay alive" — is answered by
`KeepAliveSignalName` (`turn.go`'s own doc comment has the full reasoning),
sent by `conn.go` via `core.Ingestor.KeepAlive` (`SignalWithStart`, so it can
wake an already-idled-out coordinator) every `keepAliveEvery` — derived as
`wf.IdleTTL / 3`, not a separately-tuned constant — for as long as the
connection lives. Deliberately harness-agnostic: the coordinator has no
concept of a "device" or "connection," only that something wants its idle
timer held off; there is no corresponding disconnect signal; absence of
`KeepAlive` for one `IdleTTL` window is what "nobody needs this anymore"
means, symmetric with how turn-activity idle-exit already worked. One real,
deliberate behavior change: `WriteMemoryWorkflow` (session-completion memory
consolidation) now waits for "no turn *and* no keepalive," not just
conversational idleness — the app actually closing, not just going quiet.

These two mechanisms are NOT redundant: workflow-liveness alone would be a
wrong presence proxy (`CoordinatorWorkflow` also stays alive for active-turn
reasons that have nothing to do with any connection — the device could have
disconnected mid-turn), so `mobile_presence` remains the precise source of
truth for "is anyone there," while `KeepAlive` is purely a lifecycle/scheduling
concern.

## Status pings — DONE 2026-09-12

`turn.go`'s `runProgressWatchdog`/`deliverWedgedFallback` used to gate
*writing* a status line on having an explicit push-to-one-connection
*dispatch* activity (Discord's `DiscordDeliverStatus`, routed through a
per-connection embedded worker queue) — the wrong coupling for mobile, which
never needed a dispatch step at all: migration 032/033's
`mobile_notify_status_pings` NOTIFY trigger already fires the moment
`StatusPing` writes a row. So mobile only needed `statusPingBackoff("mobile")`
to return real steps and the write to stop being gated on a dispatch
mechanism it doesn't use — both fixed; no Python change needed
(`status_ping.py` was already platform-neutral).

**Delivered as a `delta`, not a separate `status` frame** (same day,
following user direction — "treat this as a response without a request").
`emitStatus()` shares `c.lastCum` with `emitDeltas()`: a status line is just
more delta traffic through the identical cumulative-buffer/replace-on-mismatch
mechanism (`deltaFor`) already built for reconnect snapshots and provider
backtracks. When real streamed content eventually arrives, it won't extend
the status text, so it naturally supersedes it via `replace:true` — the
client needs zero new handling, and for voice, "Still working on this…" is
simply spoken as one utterance, the real answer as the next. `turn_status_pings`/
`StatusPing` themselves are unchanged, still shared with Discord's own
(structurally different) delivery. The `statusFrame` wire type is gone.

## Voice/text mode — DONE 2026-09-12

Text responses lean on markdown (tables, bold, headers) that reads badly
once spoken. Discord already solved this for its own voice platform, but by
freezing a whole alternate system prompt (`voiceSystemPromptText`) into
`sessions.system_prompt` at session genesis — a decision that fits Discord
(voice and text are structurally separate sessions) but not mobile, where
**one session serves both typing and speaking**. Mode has to travel with the
message that triggered a turn, not be fixed once.

- Migration `035`: `messages.mode` (`"voice"` | `"text"`, `NULL` = text).
- `{"type":"message", ..., "mode":"voice"}` on the client → frame → `MessageEvent.Mode`
  → `messages.mode`. Never threaded through `ModelCallInput`/the workflow
  layer — read activity-side, same reference-passing pattern as
  `speaker_id`/`client_msg_id`.
- `model_call.py` reads the turn's own most recent user message's `mode`
  and swaps in `llm.VOICE_SYSTEM_PROMPT` (a Python-side generalization of
  `prompts.go`'s `voiceSystemPromptText`, plus one new rule: the incoming
  message may itself be STT output — missing punctuation, mistranscribed
  words, filler — read it charitably rather than literally). Purely
  additive: Discord/web never send `mode`, so their behavior is completely
  unaffected; Discord voice keeps using its own session-genesis path exactly
  as before.
- Known, accepted cost: switching mode mid-session is a full system-prompt
  swap, so it breaks prompt-cache prefix reuse for that call. Unavoidable if
  the styles are meant to genuinely differ — not something to hide.

## Deferred

- **Tool-activity frames** — the app currently sees only messages + deltas +
  status, not "running search…".
