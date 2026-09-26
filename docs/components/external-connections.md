# External connections

## Implemented flow

1. `agent-web` → `/gateway/connections` on the shared router. Clerk's verified
   active organization selects the tenant; the browser never supplies an org,
   Kubernetes namespace, release name, endpoint, or raw Helm values.
2. One Postgres transaction saves desired state, optional credentials, and an
   operation (the durable outbox). Concurrent duplicate requests reuse the same
   operation. Opposing requests during an active operation return 409.
3. The automation worker starts `ConnectionProvisionWorkflow` in a dedicated
   Temporal namespace/queue. Only operation/connection IDs enter history.
4. Its activity reads the reviewed catalog plus tenant rows, constructs overrides,
   takes a Postgres advisory lock for that tenant release, and runs Helm with
   `--reuse-values --wait`. Other release values and unmanaged manifests survive.
   A matching chart name/version and a deployed release are required. Repeating
   an activity with already-applied overrides skips the upgrade.
5. The worker observes the tenant hub's `/api/backends`. The UI shows Available
   only after **the requested manifest revision** has successfully discovered and
   indexed tools, not merely after a Kubernetes rollout.
6. OAuth connections pause at Authorization needed. The authenticated router
   returns the provider URL; consent opens in a new tab. The public callback is
   limited to `/hub/{org}/oauth/{backend}/callback`; the hub validates expiring,
   single-use state and PKCE. Completion wakes that backend's polling task.
7. A background observer follows authorization completion and later availability
   changes. Disconnect removes the managed manifest; provider consent and saved
   credentials are retained, not revoked. Reconnect can reuse them.

The catalog seeds GitHub (bearer token) and Notion (OAuth). Provider permissions
and account eligibility still determine which tools each connection can access.
The current UI exposes only those reviewed configuration shapes, not arbitrary
MCP endpoints or user-supplied chart settings.

## Ownership and storage

The control API is in the **shared router**, not the tenant hub. This keeps the
control database and deployment authority outside a service that restarts during
its own reconfiguration. Tenant data APIs continue through the existing proxy.

Shared Postgres tables: `integration_catalog`, `tenant_connections`,
`tenant_connection_operations`, `tenant_connection_credentials`. Desired revision
and observed readiness are separate. Tenant Postgres still owns OAuth access and
refresh tokens and the pgvector tool index. No database-to-memory or zvec migration
is included.

The hub runs one cancellable async polling task per backend, with eight concurrent
polls, a 120-second per-poll timeout, capped failure backoff, and a separate skills
poller. One slow backend does not hold up every other backend.

## Rollout (operator action; not performed automatically)

1. Build/publish the updated hub, router, agent-web, and automation images using
   immutable tags. Automation's Dockerfile is
   `deploy/docker/connections.Dockerfile`, built with this repository as context.
   It bundles the tenant chart and its vendored dependencies; no registry login
   is needed at runtime.
2. Deploy the new hub image to the target tenants first. Keep the usual
   `<release>-tools` service name and port 8000. Existing static manifests work;
   only automation-managed ones have connection revision markers.
3. Create the dedicated Temporal namespace (default `system`) using your existing
   operator tooling. This worker does not create namespaces or onboard tenants.
4. Deploy the shared chart with the updated router/web images. Its migration hook
   applies `003_tenant_connections.sql` and `004_connection_runtime.sql` after the
   registry migrations. Ensure the router's public HTTPS URL and exact web CORS
   origin are configured. That same public URL must reach the callback route.
5. Enable `connectionAutomation.enabled`, set its image tag, and enumerate
   `connectionAutomation.allowedTenantNamespaces`. Those Kubernetes namespaces
   must exist. The chart creates a dedicated service account and a Role/RoleBinding
   in each named namespace; it grants no cluster-wide permissions. Configure
   `temporalNamespace`/`taskQueue` if not using their defaults.
6. Test GitHub connect → Available → disconnect in a nonproduction tenant, then
   Notion connect → Authorize → Available. Verify a second organization cannot see
   or change the first one's connections. Do not enable globally before this
   real-cluster/provider smoke test.

When automation is disabled, the UI is read-only and writes return 503 rather than
silently queuing work. The worker must reach shared Postgres, Temporal, Kubernetes,
and the tenant hub services. Network policies must allow these paths. For local UI
development set `VITE_DEV_ROUTER_PROXY` for `/gateway/connections`; other existing
gateway dev proxy behavior is unchanged.

## Recovery and limits

- Restarting the router/worker loses no accepted requests. Operation IDs are fixed
  workflow IDs; duplicate dispatch cannot create a second execution. Failed or
  terminated workflows are reconciled back to a retryable failure state.
- Upgrade failures are retried up to three times. A release in a pending or failed
  Helm state requires operator inspection/recovery before Retry can succeed. The
  worker deliberately does not roll back an unrelated or ambiguous revision.
- Availability is eventually consistent (UI/observer roughly every five seconds;
  provider checks at the hub's configured poll interval). A full outage may take
  one poll/timeout to appear. The workflow waits up to ten minutes after apply.
- All automated tenant upgrades use a release lock. Manual Helm operations must
  not run concurrently. Keep chart artifacts immutable: name/version matching
  cannot detect changed templates republished under the same version. A chart
  version mismatch intentionally blocks automation; rebuild against the matching
  tenant chart instead of letting a connection request upgrade unrelated software.
- Helm manages the **whole tenant release**, so its hooks and changes in the baked
  chart can affect more than the hub. Longer term, a separately versioned hub
  release/controller would reduce this scope. This implementation preserves the
  requested existing Helm deployment model.
- The worker is privileged inside allowed tenant namespaces, including access to
  Secrets. Credentials are stored in Postgres as requested; header credentials
  also appear in the existing hub manifest ConfigMap and Helm release history.
  Restrict DB/Kubernetes access and backups accordingly. Encrypted credential
  storage and Secret-backed manifests are still production hardening work.
- Any authenticated member of the active organization can manage its connections
  under the current policy. Fine-grained connection/tool authorization, approval
  for destructive tools, quotas, provider revocation and expanded audit retention
  are separate policy work, not silently implemented here.
- Keep tenant hubs internal: their existing MCP, status and OAuth-start endpoints
  are not an Internet authentication boundary. Only the narrow callback route is
  publicly forwarded without a Clerk token, and it requires valid OAuth state.
- Managed OAuth manifests carry their own callback URL; unrelated static
  integrations keep the existing hub base URL. When adopting an already-installed
  OAuth backend, its existing provider client must allow the new router callback.
  An old dynamic client registration may need operator re-registration before
  reauthorization. New connections register the correct callback automatically.

## Verification

From `workflows`, run `go test ./internal/automation/... ./internal/router/...
./cmd/connections`. Set `TEST_DATABASE_URL` to a disposable Postgres database to
also exercise transactions, concurrent requests, tenant isolation, rendering and
retry idempotency. Tests create and remove uniquely named schemas only.

In the hub repository run `python -m pytest`; store tests start disposable
pgvector containers. In agent-web run `npm run build` and lint the changed UI
files. Helm lint the shared chart with automation both disabled and enabled with
an explicit tenant namespace. These checks do not substitute for step 6 above.
