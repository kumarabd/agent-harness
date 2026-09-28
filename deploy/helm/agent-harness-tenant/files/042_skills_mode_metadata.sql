-- docs/05-architecture-domain-control-loops.md — journaling and
-- service_monitoring are no longer directly-callable one-shot skills
-- (dispatched as a nested child call with their own input_schema
-- arguments); both are now session-persistent MODES, entered via
-- tools.switch_mode and left the same way (mode_journaling.go,
-- mode_service_monitoring.go). Their `skills` table rows are KEPT, not
-- dropped — `enabled` is still the real per-tenant opt-in gate
-- (activities/activities/llm.py's ENABLED_MODES intersects this with
-- MODE_TURNS, the deployment-wide fact of which names are genuine Go
-- dispatch targets), preserving journaling's public/on-by-default and
-- service_monitoring's private/opt-in defaults from 040_skills.sql exactly.
--
-- description/input_schema are updated only so a row read directly from
-- this table (e.g. by an admin tool) doesn't describe stale, no-longer-
-- callable arguments (entry_text, service/namespace/cluster/notify_when) —
-- input_schema is otherwise unused for a mode row (switch_mode has its own
-- single schema, built from MODE_TURNS' own description text, not this
-- column); left as a trivial valid object rather than dropped, since the
-- column itself is still NOT NULL and still genuinely used by any future
-- one-shot skill row.
UPDATE skills SET
    description = 'Session-persistent journaling mode — entered via switch_mode("journaling"), not called directly. Records diary entries into the user''s Notion diary until the user is done, then switches back to chat.',
    input_schema = '{}'::jsonb
WHERE name = 'journaling';

UPDATE skills SET
    description = 'Session-persistent service-monitoring mode — entered via switch_mode("service_monitoring"), not called directly. Investigates Grafana and arms a monitoring intention for a Kubernetes service in one round, then switches back to chat.',
    input_schema = '{}'::jsonb
WHERE name = 'service_monitoring';
