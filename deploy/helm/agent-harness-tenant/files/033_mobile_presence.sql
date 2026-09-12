-- docs/components/gateway/mobile.md's "Deferred: Cross-replica presence" —
-- a real Postgres table, not a Temporal signal/Query on CoordinatorWorkflow.
-- The originally-deferred design (DeviceConnected/Disconnected signals into
-- the coordinator + a devices Query) has a real gap: a mobile client tails
-- its session via the NOTIFY trigger entirely independent of the
-- coordinator, which idles out and exits with no active turn — exactly the
-- state proactivity needs to check presence during. A signal-based design
-- would need to keep re-spinning the coordinator just to hold a connection
-- flag. Presence is connection-layer, ephemeral, non-deterministic state —
-- the same category the NOTIFY mechanism itself is (best-effort,
-- self-healing), not the durable business state Temporal workflows model.
--
-- Self-healing by construction: last_seen_at is heartbeat-updated on the
-- existing WS ping cycle (mobile/conn.go, pingEvery=25s); every read filters
-- on staleness, never trusts row existence alone. A crashed connection
-- (no clean disconnect) ages out on its own — no cleanup job, no reaper.
CREATE TABLE mobile_presence (
  session_key   text NOT NULL,
  device_id     text NOT NULL,
  replica_id    text NOT NULL,
  connected_at  timestamptz NOT NULL DEFAULT now(),
  last_seen_at  timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (session_key, device_id)
);

CREATE INDEX mobile_presence_session_idx ON mobile_presence (session_key);
