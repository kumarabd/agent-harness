# Shared client gateway: design and delivery plan

Status: CURRENT PLANNING DIRECTION, revised 2026-09-11. No runtime changes.

User requirements: sessions and histories are independent of the client; the same
authenticated user sees the same authorized session list across web and mobile.
Use production-grade approaches with minimal complexity. Breaking changes are
allowed; no legacy data migration or compatibility adapters are required.

## 1. Principle and ownership

Clients access sessions; clients do not define them. The existing sessions,
histories, parent relationships, and coordinator workflows remain authoritative.
Opening a session on another device creates a subscription, not a new session
or another execution.

| Component | Responsibility |
|---|---|
| Existing Postgres state | Sessions, histories, turns, parent links, stored input requests and results |
| Existing loop worker | Session coordination, turn execution, interrupts, branching/lifecycle mechanics |
| Shared gateway handlers | Authentication, session authorization, listing/reading sessions, submitting supported actions |
| HTTP and WebSocket adapters | Carry requests, updates, acknowledgments, and transport errors |
| Clients | Render shared state; keep local selection, cache, and connection state |

Shared handlers live inside the existing gateway process. No new service,
conversation registry, identity mapping table, or client-specific execution path.

## 2. Shared session access

Both clients ask for the same authorized session list from the existing sessions
table. They use the returned stable session identifier to open history, subscribe,
send a message, or answer a pending question.

The request means send(session_id, message). Resolve and authorize that selected
session, then submit to its existing coordinator. The adapter must not create
a different identity because the sender is web versus mobile.

The current web/mobile key split and web-only listing filter are implementation
assumptions to remove. The identifier's exact spelling is an implementation detail
in the existing shared resolver, not a reason to add another identity layer.
Session creation/default-main resolution must be consistent across clients;
ordinary reads and sends must not silently create a different session.

Validate tenant and session access before every operation. A supplied identifier
is a locator, never proof of ownership. The client surface must not narrow the
authorized list by its own origin, nor broaden access to other users or shared
channels merely because their rows exist in the same tenant database. Preserve
the existing access model; broader channel access needs an explicit policy.

Human speaker identity comes from authentication. Device ID and web/mobile origin
are connection/message metadata, separate from the human and session identities.
They do not influence which coordinator receives a message.

## 3. Reuse existing session and branch behavior

Expose existing session creation/listing and parent/branch relationships through
both adapters. Retain current loop-worker and context-seeding semantics.

This effort does not introduce finalized-turn branch cutoffs, inherited history
by reference, new LCM lineage traversal, changed context compaction, or filesystem
snapshot behavior. It does not redesign intention ownership or scheduling.

New client-visible sessions use one shared resolution path. Existing histories
need no migration, merge, backfill, or old-ID aliases. Breaking-change permission
does not authorize deleting data or cancelling work; neither is performed by
this plan.

## 4. Shared operations

These are logical operations; endpoint/frame names will be specified next.

| Operation | Shared behavior |
|---|---|
| List sessions | Same authorized list, stable IDs, and existing parent relationships on both clients |
| Read history | Read the selected session's existing public messages and turn outcomes |
| Open/subscribe | Observe the selected session; no creation or new agent execution |
| Send message | Validate access and dispatch to that session through existing ingestion |
| Answer request | Validate that the request belongs to the selected authorized session; use the existing response mechanism |
| Create/branch session | Invoke the existing session/branch behavior consistently from either client |

Both adapters call the same handlers for validation, routing, read selection,
and response shaping. HTTP responses and WebSocket frames may have different
envelopes but must agree on identifiers, content, statuses, and errors.

Preserve message IDs across retries and optimistic rendering. A retry must not
be represented as a new user message merely because its transport changed.
Document the actual backend acceptance guarantees before prescribing automatic
resend behavior. An uncertain timeout is not evidence of acceptance or rejection.

## 5. History, active state, and delivery

Build a common reader over existing stored state. It returns all public user and
assistant messages, including follow-ups within a turn, with stable IDs and order.
Return existing active-turn information, pending/resolved input requests, final
outcomes, and any available persisted partial output or progress.

Do not invent execution states or infer completion from a quiet connection.
Missing backend progress remains unavailable, rather than fabricated by an
adapter. Full model-iteration streaming and new watchdog behavior are separate
execution capabilities if the current backend cannot supply them.

Web polling and mobile WebSocket remain the initial transports. Polling reads
the shared representation; the socket tails that same representation. Either
transport can serve either client later without changing session identity.

Use bounded history pages and session-scoped cursors. Clients persist a cursor
only after applying its data locally, reconcile optimistic messages by ID, and
replace active state on reconnect. Cross-device answers must clear stale pending
questions once the shared stored state changes.

Keep the existing Postgres listener/notification approach for socket wakeups.
Catch up on connection/listener recovery, and retry failed reads even if no later
notification arrives. Socket writes must surface errors and close failed streams;
one owner manages tail state and writes. Bound send buffers and disconnect slow
readers so they can recover from stored history.

Specify poll cadence, retry/backoff, token renewal/reconnect, and foreground
refresh as transport concerns. A disconnected or backgrounded client never
cancels the session's work. OS push and presence-based notification policy are
separate features.

## 6. Concrete gateway gaps in scope

Source inspection identified these differences to address:

- **OPEN** — Web lists only web-origin sessions; mobile derives a fixed mobile
  session rather than selecting from a shared authorized list. Deferred by
  the user 2026-09-11 ("no need for now") — `core.ListSessions` is ready
  whenever this is revisited.
- **DONE 2026-09-11** (`97333c3`) — Mobile answers looked up request IDs
  without web's session-ownership check. Consolidated into
  `core.AnswerUserInput`, shared by both adapters.
- **DONE 2026-09-12** (migration `034`) — Mobile attributed the human message
  to a client-supplied device ID instead of the real (Clerk-verified) human
  identity, so `speaker_id` never actually held the human for a mobile
  message. New `messages.client_device_id` column holds the device
  separately; `speaker_id` now uniformly means the human on every platform.
  `mobile.messageFrame`/`core.HistoryMessage` carry `DeviceID` for "from your
  iPad" rendering, no longer overloading `SpeakerID` for it.
- **OPEN** — Web history returns only the first user and last assistant
  message per terminal turn. `core.ReadHistory` (built, unused) already
  returns the full list; wiring `web/poll.go` to it is blocked on a
  coordinated agent-web frontend change (same wire-shape-break reasoning as
  the session-list item above).
- **DONE 2026-09-11** (`97333c3`) — Mobile's explicit resume query was
  unbounded despite its documented replay cap. `catchup()` now paginates
  (`catchupPageSize`, loops until caught up or hits the running turn).
- **DONE 2026-09-11** (`97333c3`) — Mobile ignored write errors and modified
  tail state from multiple goroutines (confirmed via `go test -race`).
  `resumeCh` serializes all `catchup()`-touching state onto one goroutine;
  `send()` marks the connection dead on the first write failure.
- **OPEN** — The mobile live smoke test still exercises upgrade/auth
  rejection only. No authenticated, cross-client session/history/answer/
  reconnect test exists — needs a real Clerk token, which can't be minted
  from this environment (see `mobile.md`).

## 7. Separate execution-reliability findings

These remain real findings, but client unification does not by itself approve
their implementation or the previously proposed solutions:

| Finding/capability | Separate follow-up |
|---|---|
| Ingestion records dedup before Temporal signaling | Resolve acceptance failure window and define safe retry guarantees |
| InsertMessage lacks a logical insertion idempotency constraint | Verify/fix activity retry behavior |
| No explicit stop-turn primitive | **DONE 2026-09-11** — `CancelSignalName`, a dedicated signal distinct from `NewMessage`/`SignalPayload` (never a message-content command: a coordinator-level "is this text a command" scan would collide with genuine user content and still cost a ModelCall — see turn.go's own doc comment on `CancelSignalName`). Coordinator forwards it into the active Turn Workflow exactly like NewMessage forwarding; turn.go stops at the next loop-top check or mid-tool-call-batch (reusing the exact cooperative cancel()+drain path an ordinary interrupt already used) WITHOUT folding in another ModelCall — `stopReason="cancelled_by_user"`, `turns.status='cancelled'`. Exposed as `core.CancelActiveTurn` (ownership-checked, shared by both adapters): mobile's `{type:"cancel"}` frame, web's `POST /cancel`. Live-cluster test: `workflows/scenarios/test_cancel.sh` (not a run_all.sh entry — a Cancel signal doesn't fit run_scenario.sh's NewMessage-shaped harness). |
| Streaming limited to the first model call | Review backend generation/iteration streaming behavior |
| Watchdog depends on Discord delivery machinery | Review transport-independent progress production |
| Branch seeding copies current context and is best effort | Review branching semantics independently if requested |

Do not quietly discard these findings or claim production acceptance guarantees
that have not been established. Assess relevant defects before production signoff
and fix them in separately justified, bounded work. A defect can block release
without justifying the entire command-inbox or branching redesign.

## 8. Implementation sequence and verification

1. Define the shared list/select/read/send/answer contract over existing session
   identifiers and authority boundaries.
2. Extract common handlers and remove transport-based session resolution and
   listing assumptions. Reuse existing session/branch execution behavior.
3. Connect both adapters to the common handlers and read representation; handle
   bounded pagination, reconnect, and transport errors consistently.
4. Validate two authenticated clients against the same session, on different
   gateway replicas, with history, new messages, active work, and input answers.
5. Verify disconnect/reconnect, stale requests, unauthorized targets, token expiry,
   and bounded resource use. Review separate reliability findings before release.

Success means: a session created or selected in either client is visible in the
other; both show the same persisted conversation and execution; a message from
either reaches the same coordinator; opening or reconnecting never starts
duplicate work. Concurrent user messages retain the existing interrupt semantics.

Breaking cutover requires no history migration or legacy protocol support.
Coordinate clients and gateway deployment accordingly. If a separately justified
change affects shared workflows, address its active-execution compatibility as
part of that change. No blanket loop-worker rewrite is part of this plan.

Next planning artifact: the concrete shared gateway request/response contract,
grounded in the existing session and history model.

## Local evidence

Reviewed: workflows/internal/gateway/{core,web,mobile}/,
workflows/internal/workflow/{coordinator,turn,user_input,dispatch}.go,
activities/activities/{insert_message,seed_child_session,model_call}.py, and
activities/activities/lcm/. Findings describe the code inspected during planning;
proposed behavior above is not a claim of implementation.
