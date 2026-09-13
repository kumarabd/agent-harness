# Component: First-Party Realtime Gateway

> STATUS: BUILT — shared WebSocket transport for native mobile and browser
> clients. Existing mobile `/ws` clients remain compatible; Web uses
> `GET /web/ws` with polling retained as its durable recovery path.

## Role

`workflows/internal/gateway/realtime` owns the common first-party connection
machinery: first-frame Clerk authentication, resumable catch-up, durable tail
reads, live `NOTIFY` wakeups, turn/message/delta frames, cancellation, pending
user-input responses, and coordinator keepalive. It is deliberately unaware of
which client owns a connection.

Each platform adapter supplies a server-owned scope resolver:

| Route | Conversation scope | Selectable sessions |
| --- | --- | --- |
| `GET /ws` | `mobile` + authenticated user + main | No |
| `GET /web/ws` | `web` + authenticated user + requested session | Yes |
| future macOS route | `macos` + authenticated user + requested session | Yes |

Client type never comes from an inbound frame, and no adapter can use a
client-supplied full session key. This preserves intentionally separate Web,
mobile, and macOS histories.

## Recovery and delivery

The database remains authoritative. A client connects with `after_turn_seq`;
the gateway replays every later durable turn, then emits `resumed`, then tails
new rows. `NOTIFY mobile_stream` is only a wake hint, so a dropped socket or
missed notification delays delivery but cannot lose a completed turn.

Web's first-call model output now writes the same durable `turn_deliveries`
records as mobile. The realtime route tails those records as `delta` frames;
status pings are also surfaced as `delta` frames with `progress: true`.

Web opts into structured `tool_call` frames by advertising the `tool_calls`
capability in its auth frame. Each frame is a durable snapshot keyed by
`tool_call_id`, including its owning message sequence, arguments, status,
result, and timing. Inserts and state changes wake the shared tail through the
same Postgres notification channel; reconnect catch-up reads the rows again, so
tool activity has the same recovery semantics as messages. Clients replace an
earlier snapshot with a later one for the same ID.

## Wire compatibility

Mobile's existing frames remain unchanged. `auth` gains optional `session_id`,
`parent_session_id`, and `capabilities` fields. The session fields are used only
by adapters that permit session selection; the parent is genesis metadata and
defaults to Web's main session. Mobile rejects either non-main session field.
Additive outbound frames are capability-gated, so clients that omit
`capabilities` keep the original protocol. The WebSocket handshake itself is
browser-origin checked for the Web route; extra UI origins are configured with
`GATEWAY_WEB_ALLOWED_ORIGINS` as a comma-separated list.
