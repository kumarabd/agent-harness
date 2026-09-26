-- Runtime contract: credentials stay in Postgres; Temporal carries IDs only.
ALTER TABLE tenant_connections ADD COLUMN revision bigint NOT NULL DEFAULT 1;
ALTER TABLE tenant_connections ADD COLUMN tool_count integer NOT NULL DEFAULT 0;
ALTER TABLE tenant_connections ADD COLUMN observed_at timestamptz;
ALTER TABLE tenant_connection_operations ADD COLUMN revision bigint NOT NULL DEFAULT 1;
CREATE UNIQUE INDEX tenant_connection_operations_active_idx
    ON tenant_connection_operations(connection_id) WHERE status IN ('pending', 'running');
CREATE TABLE tenant_connection_credentials (
    connection_id uuid PRIMARY KEY REFERENCES tenant_connections(connection_id) ON DELETE CASCADE,
    token text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);
