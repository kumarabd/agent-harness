-- ModelCall streaming (docs/components/gateway.md's "Resolved: ModelCall
-- Streaming — Shared Infra, Text-First Rollout"). Scoped to single-shot
-- turns only (the turn's first-and-only ModelCall call, with no tool
-- calls) — a multi-iteration, tool-calling turn is completely unaffected,
-- unchanged from before this migration.

-- One row per streamed chunk, written by ModelCall as it streams and
-- consumed by DiscordDeliverChunk. `content` holds the CUMULATIVE text
-- delivered so far (through this chunk), not just this chunk's own
-- increment — Discord's edit API replaces a message's whole content, so
-- the delivery side needs the running total, not a diff to apply.
--
-- `sent` is this table's own self-contained idempotency guard, deliberately
-- NOT folded into delivered_responses, which stays exactly as it was (one
-- row per turn, for the final whole-message delivery) — a chunk and a
-- final message are genuinely different things with different identity
-- shapes, not the same ledger widened.
CREATE TABLE turn_deliveries (
  turn_id     text NOT NULL REFERENCES turns(turn_id),
  seq         int NOT NULL,
  content     text NOT NULL,
  sent        boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (turn_id, seq)
);

-- The platform message being progressively edited, for a connection-based
-- platform (Discord) — a plain opaque reference, not content, same
-- reference-passing treatment as every other ID this schema stores. Set by
-- the first chunk's delivery, read (and edited) by every later chunk.
-- NULL for a turn that was never streamed (every multi-iteration/
-- tool-calling turn, and every turn from before this migration) — a
-- non-null value here is exactly how DiscordDeliver's own end-of-turn
-- Deliver call knows to skip re-sending a message the streaming path
-- already delivered.
ALTER TABLE turns ADD COLUMN streamed_message_ref text;
