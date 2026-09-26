-- The shared router's own tenant lookup table (workflows/internal/router/
-- registry) — Clerk organization id -> which tenant's Kubernetes namespace
-- and Helm release name to reach. Deliberately NOT a values.yaml list: a
-- new tenant registering here (docs/components/gateway/web.md's onboarding
-- flow, once built) must never require redeploying this chart.
CREATE TABLE tenant_registry (
    org_id           text PRIMARY KEY,
    namespace        text NOT NULL,
    release_name     text NOT NULL,
    gateway_port     integer NOT NULL DEFAULT 8090,
    agent_brain_port integer NOT NULL DEFAULT 8080,
    created_at       timestamptz NOT NULL DEFAULT now()
);
