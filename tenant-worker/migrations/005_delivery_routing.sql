-- docs/components/gateway.md, "Resolved: Outbound Flow" / "Resolved:
-- Connection Leasing" (2026-08-25 correction) — a tenant can run more than
-- one connection of the same platform kind (e.g. two Discord bots), so the
-- connection lease has to be keyed per-connection, not per-platform.
-- connection_id is always platform-provided (Discord: the bot's own user id,
-- resolved via GET /users/@me before the lease is ever acquired), never
-- invented in config.

-- Widen from one row per platform to one row per (platform, connection_id).
-- Any pre-existing row (this tenant's single-bot Discord lease, acquired
-- under the old schema) gets connection_id = '' via the DEFAULT below —
-- harmless: the new code always acquires by a real resolved connection_id, a
-- different primary key, so that stale ''-keyed row simply expires unrenewed
-- rather than colliding with anything.
ALTER TABLE gateway_connection_leases DROP CONSTRAINT gateway_connection_leases_pkey;
ALTER TABLE gateway_connection_leases ADD COLUMN connection_id text NOT NULL DEFAULT '';
ALTER TABLE gateway_connection_leases ALTER COLUMN connection_id DROP DEFAULT;
ALTER TABLE gateway_connection_leases ADD PRIMARY KEY (platform, connection_id);
