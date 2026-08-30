-- docs/components/gateway/discord.md's "Resolved: Per-Channel Reply Mode"
-- (2026-08-30). A Discord text channel / DM can be switched between a text
-- reply and a spoken (voice-message) reply with the /mode slash command;
-- this table is where that per-channel choice lives.
--
-- Gateway-owned, like every other gateway-observed fact (gateway_connection_
-- leases, ingested_messages, discord_ambient_messages): the /mode command
-- handler writes it, and the DiscordDeliver / DiscordDeliverChunk activities
-- (which run on the gateway's own embedded worker, against the gateway's own
-- Postgres connection) read it. The turn workflow never sees it — delivery
-- modality is a gateway-plane concern, not part of the shared turn record,
-- the same split discord-voice.md's own lifecycle states already draw.
--
-- Keyed by channel_id, NOT session_key: a channel can hold several sessions
-- (reply-chain branches, gateway.md's "Resolved: Multi-Session Channels"),
-- and the reply mode is a property of the channel the human is talking in,
-- not of any one conversation thread within it. Absent row = 'text' (the
-- pre-2026-08-30 behavior); nothing backfills existing channels.
CREATE TABLE discord_reply_mode (
    channel_id text PRIMARY KEY,
    mode       text NOT NULL CHECK (mode IN ('text', 'voice')),
    updated_at timestamptz NOT NULL DEFAULT now()
);
