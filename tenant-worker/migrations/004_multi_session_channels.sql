-- docs/components/gateway.md, "Resolved: Multi-Session Channels" and
-- docs/components/gateway/discord.md — a channel can hold more than one
-- session (a reply-threaded conversation, distinct from the channel's main
-- one). Phase 1 of the Discord gateway build: schema only, generic pieces
-- first.

-- Records which session a session branched from, when it did — the session
-- graph as durable data, not a Temporal parent/child workflow relationship
-- (gateway.md's own reasoning for why: CoordinatorWorkflow's idle-timeout/
-- recreate lifecycle can't function as a stable Temporal "parent"). NULL for
-- a channel's own main session. Self-referencing FK for integrity — a
-- session's parent, if set, must itself be a real session.
ALTER TABLE sessions ADD COLUMN parent_session_key text REFERENCES sessions(session_key);

-- docs/components/gateway.md, "Resolved: Connection Leasing" — reuses
-- session_filesystem_leases/leases.py's exact renewal-based compare-and-swap
-- pattern (see that table's own migration) as a new lease kind on
-- infrastructure that already exists. One row per platform this tenant's
-- Gateway has a live connection for; tenant is implicit (this table lives in
-- that tenant's own Postgres).
CREATE TABLE gateway_connection_leases (
  platform    text PRIMARY KEY,
  holder_id   text NOT NULL,
  acquired_at timestamptz NOT NULL DEFAULT now(),
  expires_at  timestamptz NOT NULL  -- renewed periodically by whoever holds it, not a fixed TTL
);

-- docs/components/gateway/discord.md, "Resolved: Ambient Message Buffer" —
-- every message in a channel the bot is in, whether or not it ever triggers
-- a real turn. Pure Postgres inserts from the Gateway process directly, no
-- SignalWithStart, no Coordinator, no Temporal workflow involved for an
-- ambient message — see that doc for why (avoids both per-message Temporal
-- overhead and a real interrupt-forwarding bug the "every ambient message is
-- its own turn" alternative would have needed surgery on coordinator.go to
-- avoid). Also the source data reply-chain resolution walks to compute a
-- MessageEvent's Discriminator.
CREATE TABLE discord_ambient_messages (
  id                            bigserial PRIMARY KEY,
  channel_id                    text NOT NULL,
  platform_message_id           text NOT NULL,
  reply_to_platform_message_id  text,          -- Discord's own message_reference, when present
  author                        text NOT NULL, -- MessageEvent.User equivalent
  content                       text NOT NULL,
  created_at                    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX ON discord_ambient_messages (channel_id, platform_message_id);
CREATE INDEX ON discord_ambient_messages (channel_id, created_at);
