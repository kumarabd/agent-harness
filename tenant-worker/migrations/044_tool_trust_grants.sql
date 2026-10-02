-- docs/components/user-input.md's "Resolved: Always-Allow Trust Grants" —
-- same {server, tool} identity vocabulary as PermissionRule (permissions.py)
-- and search_tools/call_tool, kept exactly as decoupled from shell_hub.py/
-- mcp_hub.py as PERMISSION_LIST itself already is: a grant is extrinsic
-- deployment policy, not a tool-catalog property. No tenant column — each
-- tenant has its own Postgres instance, so this table is naturally
-- tenant-scoped by topology, same as every other table in this schema.
CREATE TABLE tool_trust_grants (
  server                text NOT NULL,
  tool                  text NOT NULL,
  granted_at            timestamptz NOT NULL DEFAULT now(),
  -- Audit trail: which approval click granted blanket trust. Nullable
  -- rather than NOT NULL — a future non-UI path to seed a grant (e.g. an
  -- operator pre-trusting a tool) shouldn't be forced to fabricate a
  -- request_id that never existed.
  granted_by_request_id text REFERENCES user_input_requests(request_id),
  PRIMARY KEY (server, tool)
);
