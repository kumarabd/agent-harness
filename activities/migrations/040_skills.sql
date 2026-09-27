-- docs/05-architecture-domain-control-loops.md, docs/components/turn-pipeline.md
-- ("Skills") — replaces activities/activities/skills.py's hardcoded SKILLS
-- list and workflows/internal/workflow/skills/registry.go's hand-synced Go
-- mirror with one per-tenant source of truth. Same precedent mcp-hub's own
-- `connections` table already set for backend config (static YAML manifests
-- -> per-tenant Postgres-backed catalog, 2026-09-26): this tenant's own
-- Postgres already sits behind both real consumers (tenant-worker via
-- skills.init(), Gateway's own GET /skills), so there is no new cross-
-- service plumbing needed, and per-tenant `enabled` becomes possible for the
-- first time — every tenant's image used to ship the exact same hardcoded
-- global list, with no way to turn one off for a single tenant without a
-- code change.
--
-- name is also the Temporal workflow type (turn.go dispatches
-- ExecuteChildWorkflow by this string directly, matching skills.py's own
-- former "name IS the Temporal workflow type" collapse) — still hand-authored
-- only, per skills.py's original docstring: a row here without a matching
-- registered Go workflow type just fails to dispatch, same as today.
--
-- visibility distinguishes the DEFAULT `enabled` a new/upgraded tenant gets,
-- nothing more — `enabled` is still the one thing anything at runtime
-- actually reads (skills.init's WHERE enabled). 'public' skills (journaling)
-- are broadly useful, work the same way for every tenant, and are seeded
-- enabled=true; 'private' skills (service_monitoring — meaningful only for a
-- tenant that actually runs Kubernetes workloads it wants watched, and
-- reaches into cluster/Grafana access most tenants won't have at all) are
-- seeded enabled=false, opt-in per tenant. Named "visibility", not "group" —
-- GROUP is a reserved SQL keyword and would need quoting at every call site.
-- Not column-enforced (whoever hand-authors a new row sets both fields
-- deliberately, same "hand-authored only" posture as everything else here)
-- — a CHECK constraint just catches a typo'd value, not a mismatched pair.
CREATE TABLE skills (
    name TEXT PRIMARY KEY,
    description TEXT NOT NULL,
    input_schema JSONB NOT NULL,
    visibility TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('public', 'private')),
    enabled BOOLEAN NOT NULL DEFAULT true,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Seeded, not left empty: every existing tenant keeps the exact two skills
-- it already had, with no action required on upgrade — journaling public
-- and already enabled (matching its pre-migration behavior, where every
-- tenant's image shipped it unconditionally); service_monitoring private and
-- now OFF by default (a behavior change from pre-migration, deliberately —
-- see above), until a tenant opts in.
INSERT INTO skills (name, description, input_schema, visibility, enabled) VALUES
(
    'journaling',
    $desc$Record a journal entry the user has decided to keep, into today's page in their Notion diary: a page titled My Diary with one YYYY-MM-DD child page per day. It may be invoked when the user explicitly asks or when the agent judges a thought worth preserving, but the skill asks before recording it.$desc$,
    $json${"type":"object","properties":{"entry_text":{"type":"string","description":"The journal entry's content."}},"required":["entry_text"]}$json$::jsonb,
    'public',
    true
),
(
    'service_monitoring',
    $desc$Configure durable monitoring for a Kubernetes service. This skill uses Grafana as the mandatory source of monitoring evidence: it finds the relevant Grafana capability, inspects the service's actual metrics, dashboards, and existing alerts, selects an appropriate health signal, asks before making consequential external changes, and arms a monitoring intention. Use this when the user asks to watch, monitor, or be alerted about a Kubernetes service.$desc$,
    $json${"type":"object","properties":{"service":{"type":"string","description":"Kubernetes service or workload to monitor."},"namespace":{"type":"string","description":"Optional Kubernetes namespace containing the service."},"cluster":{"type":"string","description":"Optional cluster name or identifier."},"notify_when":{"type":"string","description":"What should trigger notification; defaults to the service being unavailable."}},"required":["service"]}$json$::jsonb,
    'private',
    false
);
