# Component: State Layer
## (Postgres — Sessions, Messages, Routing)

> STATUS: SCAFFOLD — to be filled in as we design.

### Role (one line)
The shared networked datastore that replaces Hermes' single SQLite file, giving many gateways and workers real row-level concurrency and a durable record of sessions, messages, and routing.

### Responsibilities (from architecture)
- Store sessions (metadata: model, system_prompt, token_count, title, cwd/branch/repo where relevant).
- Store messages (role, content, tool_calls) per turn.
- Support full-text search across messages (Hermes uses FTS5 today; Postgres equivalent to design).
- Hold the gateway routing index (maps session keys to active sessions).
- Serve as the durable record written by the persist activity and read by workers to load context.
- Hold the **session filesystem lease/metadata index** — one row per session/subagent-scoped resource on the shared mount (path, content hash, last-writer, lease holder + expiry) — the coordination layer that lets stateless workers safely share session working directories without worker pinning. See `components/session-filesystem.md`.
- Hold the **deliver idempotency ledger** (`delivered_responses(response_id, delivered_at)`) — checked-then-inserted by the gateway immediately before the actual platform send call, keyed by the turn workflow ID, so at-least-once redelivery (from Temporal retry or a gateway crash) never produces a duplicate user-visible send. See `components/activities-outbound-delivery.md`.

### Key Design Decisions (recap)
- Replace SQLite with Postgres to remove the serialized-write wall and corruption risk under multi-process load.
- Do NOT share a SQLite file across processes — that is the exact corruption failure mode in the Hermes RFC.
- Filesystem coordination (leases) belongs here, not on the shared mount itself — NFS-style mounts don't give reliable atomic locking across clients, so Postgres's compare-and-swap semantics are the actual lock, with the mount as pure byte/hierarchy storage. Leases use renewal-based expiry (not a fixed TTL), matching the cooperative-cancellation pattern already used for activities.

### Resolved: Schema

**Core design choice: identity is borrowed from Temporal, not re-minted in Postgres.** `components/temporal-workflow.md` already commits to workflow IDs that mirror the execution tree (`{session_key}:turn:{n}`, extended to `:sub:{m}` for subagents). Rather than generating a parallel UUID for the same execution unit and having to keep two IDs in sync, Postgres primary keys **are** the Temporal workflow/activity IDs verbatim. One identity per execution unit, not two — this also means "only the parent id matters": every table below needs exactly one `parent_id` column and no separate `child_workflow_id`/foreign-key-to-a-different-ID-space anywhere.

```sql
-- One session coordinator lifetime. Not one row per coordinator process restart —
-- those recreate freely under the same session_key (see components/session-coordinator.md).
CREATE TABLE sessions (
  session_key     text PRIMARY KEY,        -- e.g. "agent:main:discord:guild:123"
  platform        text NOT NULL,
  channel_id      text NOT NULL,
  model           text,
  system_prompt   text,
  title           text,
  cwd             text,                    -- coding-session metadata, nullable
  repo            text,
  branch          text,
  created_at      timestamptz NOT NULL DEFAULT now(),
  last_active_at  timestamptz NOT NULL DEFAULT now(),  -- updated by persist activity per turn
  last_consolidated_turn_seq int  -- watermark for incremental session consolidation (components/session-consolidation.md)
);

-- Every workflow-shaped execution unit: top-level turns AND subagent turns, since
-- they're recursively the same workflow type. turn_id IS the Temporal workflow ID.
CREATE TABLE turns (
  turn_id       text PRIMARY KEY,          -- "{session_key}:turn:{n}" or "...:sub:{m}"
  parent_id     text NOT NULL,             -- a session_key OR another turn_id
  parent_type   text NOT NULL CHECK (parent_type IN ('session', 'turn')),  -- parent_id is polymorphic
  turn_seq      int,                       -- only meaningful when parent_type = 'session'
  status        text NOT NULL CHECK (status IN ('running', 'completed', 'cancelled', 'failed')),
  started_at    timestamptz NOT NULL DEFAULT now(),
  completed_at  timestamptz
);
CREATE INDEX ON turns (parent_id);
-- Enforces the coordinator's turn_seq semantics: two top-level turns for the same
-- session can never collide. Partial index since turn_seq is only meaningful when
-- parent_type = 'session' (subagent turns don't have one).
CREATE UNIQUE INDEX turns_session_turn_seq_uidx ON turns (parent_id, turn_seq)
  WHERE parent_type = 'session';

-- Messages are the one non-Temporal entity — no workflow/activity ID to borrow —
-- so they get a lightweight synthetic key. parent_id = turn_id is sufficient;
-- session is one join away (turns.parent_id, walked up for nested subagents),
-- never duplicated.
CREATE TABLE messages (
  message_id     uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id      text NOT NULL REFERENCES turns(turn_id),
  role           text NOT NULL CHECK (role IN ('user', 'assistant', 'tool')),
  content        text,
  seq            int NOT NULL,             -- ordering within the turn
  created_at     timestamptz NOT NULL DEFAULT now(),
  search_vector  tsvector GENERATED ALWAYS AS (to_tsvector('english', coalesce(content, ''))) STORED
);
CREATE INDEX ON messages (parent_id);
CREATE INDEX messages_search_idx ON messages USING GIN (search_vector);

-- One row per tool call. tool_call_id IS the Temporal activity ID, fully qualified
-- as "{turn_id}:act:{n}" since raw activity IDs are only unique within one workflow
-- execution. Under the reference-passing contract (components/temporal-workflow.md,
-- revised 2026-08-14), ModelCall mints this ID and writes this row as part of
-- producing its response — not the workflow assigning it when starting the
-- activity, since ModelCall is the one holding the arguments this row needs.
-- The workflow receives the ID back in ModelCall's output and uses it, unmodified,
-- as the ActivityID when it starts the corresponding ToolCall activity.
-- For a subagent call, tool_call_id IS the child workflow's turn_id directly —
-- no separate child_workflow_id column; the row's own primary key already is that ID.
CREATE TABLE tool_calls (
  tool_call_id   text PRIMARY KEY,         -- "{turn_id}:act:{n}", OR a child turn_id if is_subagent
  parent_id      text NOT NULL REFERENCES turns(turn_id),  -- the turn/subagent that issued it
  message_id     uuid NOT NULL REFERENCES messages(message_id),  -- the assistant message that issued it
  tool_name      text NOT NULL,
  arguments      jsonb NOT NULL,
  retry_hint     jsonb,                    -- optional, protocol-level (components/activities-outbound-delivery.md)
  is_subagent    boolean NOT NULL DEFAULT false,
  status         text NOT NULL CHECK (status IN ('ok', 'error', 'cancelled')),
  reason         text,                     -- e.g. 'interrupted_by_new_message'
  side_effect    text CHECK (side_effect IN ('none', 'partial', 'unknown')),  -- only when status='cancelled'
  result         jsonb,
  partial_output text,
  started_at     timestamptz NOT NULL DEFAULT now(),
  completed_at   timestamptz
);
CREATE INDEX ON tool_calls (parent_id);

-- Filesystem lease/metadata index (components/session-filesystem.md).
CREATE TABLE session_filesystem_leases (
  session_key   text NOT NULL REFERENCES sessions(session_key),
  path          text NOT NULL,             -- e.g. '/session/{key}/' or '/session/{key}/sub/2/'
  holder_id     text NOT NULL,             -- worker/activity identity
  acquired_at   timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,      -- renewed periodically, not a fixed TTL
  content_hash  text,
  last_writer   text,
  PRIMARY KEY (session_key, path)
);

-- Deliver idempotency ledger (components/activities-outbound-delivery.md).
-- response_id = turn_id directly, since one turn produces exactly one final response.
CREATE TABLE delivered_responses (
  response_id   text PRIMARY KEY REFERENCES turns(turn_id),
  delivered_at  timestamptz NOT NULL DEFAULT now()
);
-- Retention: functionally safe to prune once the referenced turn is older than the
-- delivery pipeline's max plausible redelivery window (e.g. 24h) — but deliberately
-- left unbounded for now. Row is tiny (two columns) and the only access pattern is a
-- PK point lookup, which stays fast regardless of table size — no actual storage or
-- performance cost to leaving it unpruned. See future-work.md for the deferred item.

-- Inbound dedup ledger (components/gateway.md). Symmetric to delivered_responses
-- on the outbound side: webhook platforms redeliver at-least-once, so the gateway
-- checks-then-inserts before calling SignalWithStart, to avoid double-signaling.
CREATE TABLE ingested_messages (
  platform             text NOT NULL,
  platform_message_id  text NOT NULL,
  session_key           text NOT NULL REFERENCES sessions(session_key),
  ingested_at           timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (platform, platform_message_id)
);

-- Per-shard resume state for connection-based platforms (components/gateway.md).
-- Tiny and cheap: just enough to let a replacement pod (same StatefulSet ordinal,
-- same shard identity) resume the platform's own session rather than reconnecting
-- cold, shrinking the inbound-loss window on a worker crash. NOT used for shard
-- ownership/assignment itself — that's static config + StatefulSet ordinal, not
-- a Postgres fact. Deliberately not stored on local pod disk (see components/gateway.md
-- for why that was rejected — same reasoning as session-filesystem's worker-pinning
-- rejection).
CREATE TABLE gateway_shard_state (
  platform        text NOT NULL,
  shard_id        int NOT NULL,
  last_sequence   bigint,
  updated_at      timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (platform, shard_id)
);

-- Test-fixture-only, never touched in a real deployment. Written by the
-- starter CLI (workflows/cmd/starter) before SignalWithStart, read by
-- ModelCall's own implementation — never passed through the workflow, so
-- the fixture mechanism doesn't itself violate the reference-passing
-- contract it exists to exercise (components/temporal-workflow.md,
-- "Resolved: Reference/ID Schema").
CREATE TABLE _test_scripted_responses (
  turn_id    text NOT NULL REFERENCES turns(turn_id),
  seq        int NOT NULL,               -- matches ModelCallInput.context_seq
  content    text NOT NULL,
  tool_calls jsonb NOT NULL DEFAULT '[]', -- [{tool_name, is_subagent, arguments}], written straight to tool_calls by ModelCall
  usage      jsonb NOT NULL DEFAULT '{"input_tokens":0,"output_tokens":0}',
  PRIMARY KEY (turn_id, seq)
);
```

**Removed: `session_routing`.** Originally designed to store which gateway instance owns a session, for the deliver activity to address. Superseded by `components/gateway.md`'s resolved design: delivery is dispatched via a Temporal task queue computed *deterministically* from static shard config (or just platform, for webhook-based platforms) — a computation, not a stored, potentially-stale fact. No routing table needed.

**Full-text search:** Postgres-native `tsvector`/GIN, generated column on `messages.content` — resolves the "tsvector vs. external" question in favor of native, reasonable at single-user scale; revisit only if search quality demands more (e.g. `pg_trgm`, or an external engine) later.

**Write pattern / concurrency:** every ID in this schema (`turn_id`, `tool_call_id`) is a stable Temporal ID, not an auto-increment, so every write below is naturally idempotent (a retry upserts the same row, doesn't duplicate it) — consistent with the durability-backbone reasoning already established for persist (`components/activities-outbound-delivery.md`). **Revised 2026-08-14, no longer one single end-of-turn transaction:** under the reference-passing contract, writes to `messages`/`tool_calls` happen incrementally across the turn, not batched — a start-of-turn activity writes the inbound message before the first `ModelCall`; each `ModelCall` writes its own response + any `tool_calls` rows as part of producing its reference-only output; `Persist` narrows to whatever hasn't already been written that way (see the read/write table below for exactly which activity owns which write). Default `READ COMMITTED` isolation is still sufficient: by construction, only one turn workflow drives writes for a given session at a time, so there's no cross-session write contention to design around — this didn't change, only the granularity of individual writes did.

**Deliberately not done:** no `ltree`/materialized-path querying, despite the IDs encoding the full ancestry chain as a string. Every real access pattern here is one level (turn's messages, session's turns) — never "all descendants at any depth" in a single query — so a plain adjacency list (`parent_id`) fully covers it. Reaching for path-query machinery here would be solving a query pattern that doesn't exist yet.

### Resolved: Read/Write Split Per Writer
Not a new design decision — mostly making explicit what earlier docs already implied about who touches Postgres, so it isn't left to inference.

| Table | Written by | Read by |
|---|---|---|
| `sessions` | **Gateway** — upserts the row on first contact for a session key (creates if absent). **Persist activity** — updates `last_active_at` on every turn completion. **Session consolidation job** — advances `last_consolidated_turn_seq` after a successful episode push (`components/session-consolidation.md`). | Turn workflow's context-hydration activity (session metadata: model, system_prompt, cwd/repo/branch). Consolidation job (scans for idle, not-yet-consolidated sessions). |
| `turns` | **Turn workflow**, via its own activities — inserts the row (`status='running'`) when the turn starts; the **persist activity** updates it to `completed`/`cancelled`/`failed` at the end. | **Session coordinator** — the one Postgres read it performs, `SELECT MAX(turn_seq)+1 ... WHERE parent_id=$session_key AND parent_type='session'`, to recompute the next turn_seq on startup (see `components/session-coordinator.md`). Turn workflow's context-hydration activity (recent turn history). |
| `messages` | **Revised 2026-08-14 — incremental, not end-of-turn-only.** A start-of-turn activity inserts the inbound message (sourced from the coordinator's signal payload) *before* the first `ModelCall`, per the reference-passing contract (`components/temporal-workflow.md`). Each `ModelCall` inserts its own assistant-message row as part of producing its reference-only output. `Persist` now only covers whatever's left at turn end (e.g. tool/observation messages folded in after cancellation). | `ModelCall` (reads prior turn history to build context — this *is* the context-hydration step now, not a separate activity). Future: consolidation job (`future-work.md` §3). |
| `tool_calls` | **Revised 2026-08-14 — written by `ModelCall`, not `Persist`.** `ModelCall` mints each `tool_call_id` and writes the row (including `arguments`) as part of producing its response, since it's the one holding the content that row needs. The `ToolCall` activity updates the same row with `status`/`result`/`reason`/`side_effect` once it completes (or is cancelled) — two writers to one row over its lifecycle, not two separate rows. | `ModelCall` (reads `tool_call_id`s back when relevant — e.g. subagent merge-back). Future: consolidation job. |
| `session_filesystem_leases` | Whichever tool-call activity currently holds (or is renewing/releasing) a lease — see `components/session-filesystem.md`. | Any tool-call activity attempting to acquire a lease on the same path. |
| `delivered_responses` | **Gateway** — checked-then-inserted immediately before the real platform send call (`components/activities-outbound-delivery.md`). | Gateway (the same check, on redelivery). |
| `ingested_messages` | **Gateway** — checked-then-inserted before calling `SignalWithStart`, to dedup at-least-once webhook redelivery (`components/gateway.md`). | Gateway (the same check, on redelivery). |
| `gateway_shard_state` | **Gateway** (connection-based platforms only) — updated as new sequence numbers are acknowledged on a shard's connection. | Gateway, on reconnect/resume after a crash or restart. |

**Net shape:** the gateway only ever writes what it directly observes and owns operationally (dedup ledgers, shard resume state) — it never writes turn/tool_call/message outcomes, those stay owned end-to-end by the turn workflow's own persist activity, sourced from the signal payload rather than a separate gateway write. This matches the inbound/outbound path separation already established in `02-architecture-temporal-execution.md` §4, and is slightly tighter than an earlier version of this table, which had the gateway writing `messages` directly — removed once `components/gateway.md` resolved that as redundant (see that doc's Inbound Flow).

### Open Questions / To Design
- Migration path / relationship to the upstream Hermes pluggable-SessionDB RFC — deliberately not pursuing; noted for completeness only.
- Multi-tenant scoping — resolved in `components/multi-tenancy.md`: **revised 2026-08-14** from "separate database per tenant" (implying a shared/managed Postgres service) to "one self-hosted Postgres instance per tenant, backed by that tenant's own PersistentVolume" (`components/session-filesystem.md`) — avoids any unified Postgres layer across tenants, not just a unified database. Affects deployment topology only; the schema above is unchanged either way.

### Notes Log
- 2026-08-07: Added the session filesystem lease/metadata index as a state-layer responsibility, resolving subagent filesystem coordination without worker pinning — see new `components/session-filesystem.md`.
- 2026-08-07: Added the `delivered_responses` idempotency ledger as a state-layer responsibility, resolving deliver-path double-send safety — see `components/activities-outbound-delivery.md`.
- 2026-08-07: Resolved the full schema. Core decision: primary keys are Temporal workflow/activity IDs verbatim rather than separately-minted UUIDs — `turns.turn_id` is the workflow ID, `tool_calls.tool_call_id` is the (fully-qualified) activity ID or, for subagents, the child workflow's own turn_id directly. This collapses "only the parent id matters" into the schema itself: one `parent_id` per table, no redundant child-reference columns. `turns` covers top-level turns and subagents in one self-referencing table via a `parent_type` discriminator (session vs. turn), since they're the same workflow type recursively. FTS resolved as native `tsvector`/GIN. Write pattern resolved as per-turn upsert-by-primary-key, `READ COMMITTED`, no cross-session contention by construction.
- 2026-08-07: Closed out two remaining schema gaps: (1) added a partial unique index (`turns_session_turn_seq_uidx`) enforcing that `turn_seq` can't collide within a session, scoped to `parent_type='session'` since subagent turns don't have one; (2) enumerated the full read/write split per writer (gateway vs. turn workflow's activities vs. session coordinator) as an explicit table, confirming the gateway only ever writes what it directly observes and never touches turn/tool_call outcomes. Deliberately skipped wiring `delivered_responses` pruning to a schedule, and dropped the Hermes-RFC migration question as not worth pursuing.
- 2026-08-07: Schema changes from the gateway design pass (`components/gateway.md`): removed `session_routing` (delivery targeting is now a deterministic computation from static shard config, not a stored fact); added `ingested_messages` (inbound webhook dedup ledger, symmetric to `delivered_responses`) and `gateway_shard_state` (tiny per-shard last-sequence-number for platform session-resume on reconnect — deliberately in Postgres, not local pod disk, for the same reason session-filesystem rejected worker-pinned state). Also corrected the `messages` write-owner: the gateway no longer writes messages directly — that redundant write was removed once `SignalWithStart`'s own durable payload was recognized as sufficient; the persist activity now sources the inbound message from the signal payload.
- 2026-08-07: Added `sessions.last_consolidated_turn_seq` — a single watermark column, not a new table — supporting incremental session consolidation. See new `components/session-consolidation.md`.
- 2026-08-14: Noted the deployment-topology revision from `components/multi-tenancy.md` — Postgres is now one self-hosted instance per tenant on that tenant's PV, not a database on a shared/managed service. Schema unchanged; also noted that activities reading/writing this schema now do so by reference from the workflow (`components/temporal-workflow.md`'s reference-passing contract) rather than the workflow carrying content directly.
- 2026-08-14: Settled the reference-passing contract's concrete effect on this schema: `messages`/`tool_calls` writes move from one end-of-turn transaction to incremental writes across the turn (start-of-turn message insert before the first `ModelCall`; `ModelCall` itself writes its own response row and mints+writes `tool_calls` rows including `arguments`; `Persist` narrows to whatever's left at turn end). Added `_test_scripted_responses`, a test-fixture-only table that keeps the starter CLI's scripted-response mechanism from itself violating the contract it's meant to demonstrate.
