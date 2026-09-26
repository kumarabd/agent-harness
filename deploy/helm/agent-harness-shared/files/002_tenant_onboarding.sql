-- Self-serve tenant onboarding (docs/components/gateway/web.md's Phase 2) —
-- the automation worker's TenantOnboardingWorkflow and the router's own
-- POST /onboard handler share these two tables. Same Postgres instance/
-- database as 001_tenant_registry.sql (this chart's control-plane DB, never
-- a tenant's own data).

-- One row per onboarding request, keyed by a client-generated request_id
-- (the router's POST /onboard handler dedups on this via
-- ON CONFLICT DO NOTHING, same idempotency pattern as the Gateway's own
-- ingested_messages table). The partial unique index on tenant_slug is a
-- SEPARATE guard: it stops two different requests from racing to claim the
-- same tenant slug while either is still pending/running, regardless of
-- request_id.
CREATE TABLE tenant_onboarding_requests (
    request_id        uuid PRIMARY KEY,
    requester_user_id text NOT NULL,
    tenant_slug       text NOT NULL,
    status            text NOT NULL DEFAULT 'pending',
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    error             text,
    CONSTRAINT tenant_onboarding_requests_status_check
        CHECK (status IN ('pending', 'running', 'awaiting_approval', 'completed', 'failed'))
);
CREATE UNIQUE INDEX tenant_onboarding_requests_slug_active_idx
    ON tenant_onboarding_requests (tenant_slug)
    WHERE status IN ('pending', 'running', 'awaiting_approval');

-- Per-step progress, written by the workflow's own activities (each step
-- writes running -> done/failed) — the same "activities write progress
-- rows a poller re-reads" shape workflows/internal/gateway/realtime already
-- uses for turn-status streaming, new table instead of turn_status_pings.
-- A NOTIFY on tenant_onboarding_progress accompanies every insert (payload:
-- request_id) for a future WS tail (docs/components/gateway/web.md's Phase
-- 3) — unused by anything yet, harmless to have in place now.
CREATE TABLE tenant_onboarding_steps (
    id          bigserial PRIMARY KEY,
    request_id  uuid NOT NULL REFERENCES tenant_onboarding_requests (request_id),
    step        text NOT NULL,
    status      text NOT NULL,
    message     text,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_onboarding_steps_status_check
        CHECK (status IN ('running', 'done', 'failed'))
);
CREATE INDEX tenant_onboarding_steps_request_idx ON tenant_onboarding_steps (request_id, id);
