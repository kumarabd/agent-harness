-- External connection control plane. This shared database owns desired state
-- and provisioning progress; individual tenant hubs keep their runtime OAuth
-- tokens and discovered tools in their own database.

CREATE TABLE integration_catalog (
    integration_id       text PRIMARY KEY,
    display_name         text NOT NULL,
    description          text NOT NULL,
    icon                 text,
    auth_kind            text NOT NULL,
    manifest_template    jsonb NOT NULL,
    enabled              boolean NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT integration_catalog_auth_kind_check
        CHECK (auth_kind IN ('none', 'header', 'oauth', 'client_credentials'))
);

CREATE TABLE tenant_connections (
    connection_id        uuid PRIMARY KEY,
    org_id               text NOT NULL REFERENCES tenant_registry(org_id),
    integration_id       text NOT NULL REFERENCES integration_catalog(integration_id),
    desired_state        text NOT NULL DEFAULT 'connected',
    state                text NOT NULL DEFAULT 'pending',
    config               jsonb NOT NULL DEFAULT '{}',
    secret_ref           text,
    hub_backend_name     text NOT NULL,
    workflow_id          text,
    error                text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    UNIQUE (org_id, integration_id),
    CONSTRAINT tenant_connections_desired_state_check
        CHECK (desired_state IN ('connected', 'disconnected')),
    CONSTRAINT tenant_connections_state_check
        CHECK (state IN ('pending', 'provisioning', 'needs_authorization', 'indexing', 'ready', 'failed', 'disconnecting', 'disconnected'))
);
CREATE INDEX tenant_connections_org_idx ON tenant_connections (org_id, updated_at DESC);

CREATE TABLE tenant_connection_operations (
    operation_id         uuid PRIMARY KEY,
    connection_id        uuid NOT NULL REFERENCES tenant_connections(connection_id) ON DELETE CASCADE,
    kind                 text NOT NULL,
    status               text NOT NULL DEFAULT 'pending',
    workflow_id          text,
    message              text,
    error                text,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT tenant_connection_operations_kind_check CHECK (kind IN ('connect', 'disconnect', 'retry')),
    CONSTRAINT tenant_connection_operations_status_check CHECK (status IN ('pending', 'running', 'completed', 'failed'))
);
CREATE INDEX tenant_connection_operations_connection_idx ON tenant_connection_operations (connection_id, created_at DESC);

-- Seed only non-sensitive, reviewed integrations. The provisioning worker
-- renders these typed templates; it never accepts raw Helm values from the UI.
INSERT INTO integration_catalog (integration_id, display_name, description, icon, auth_kind, manifest_template)
VALUES
  ('notion', 'Notion', 'Search and work with your team workspace.', 'notion', 'oauth', '{"name":"notion","transport":{"type":"http","url":"https://mcp.notion.com/mcp"},"auth":{"type":"oauth"}}'),
  ('github', 'GitHub', 'Read and manage repositories, issues, and pull requests.', 'github', 'header', '{"name":"github","transport":{"type":"http","url":"https://api.githubcopilot.com/mcp/"},"auth":{"type":"header"}}')
ON CONFLICT (integration_id) DO NOTHING;
