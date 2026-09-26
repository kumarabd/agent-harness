# External connections

## Resolved: mcp-hub owns connections entirely (2026-09-26)

Earlier designs (both a shared cross-tenant Postgres/Temporal-workflow
version, and a later per-tenant Go "connections" service that shelled out
to `helm upgrade` to inject mcp-hub manifest config) are gone. mcp-hub
itself now owns connection management end to end — there is no
agent-harness-side service, database, or workflow for this at all anymore.

## Implemented flow

1. `agent-web` calls the shared router's `/connections/` prefix
   (`src/lib/gateway.ts`'s `CONNECTIONS_BASE`). The router verifies the
   caller's Clerk JWT, resolves their tenant by convention
   (`tenantid.SlugForSub`), and reverse-proxies straight through to that
   tenant's own mcp-hub instance
   (`workflows/internal/router/core/tenant.go`'s `McpHubBaseURL()`,
   `http://<release>-tools.<namespace>.svc.cluster.local:8000`) —
   the exact same pattern as `/gateway/` and `/brain/`.
2. mcp-hub serves the real API directly:
   - `GET /api/catalog` — the reviewed, buildable-from-a-name templates
     (`mcp-hub/src/mcp_hub/catalog.py`): `notion` (OAuth, browser consent),
     `github`/`exa` (header token), `trek` (OAuth `client_credentials` — a
     machine client, no browser step, needs `client_id`/`client_secret`
     supplied directly), and `abrp`/`finance`/`health`/`maps-engine`/
     `grafana` (no secret at all — connecting is a single click). Each
     entry's `auth_kind`/`oauth_grant_type` tells `agent-web`'s connections
     page which input fields to show.
   - `GET /api/connections` — every currently-configured backend (catalog
     ones and ops-managed ones alike) with live health/status merged in.
   - `POST /api/connections` — activate a catalog entry (`{name, token?}`)
     or register an arbitrary backend (`{name, url, auth_kind, headers?,
     oauth?}`) — the same endpoint serves both the end-user "connect
     Notion" flow and an operator wiring up an internal tool.
   - `DELETE /api/connections/{name}` — deactivate, cascading its indexed
     tools and OAuth/health rows.
   All of it is backed by mcp-hub's own Postgres `connections` table
   (`mcp-hub/src/mcp_hub/store.py`'s `ConnectionRecord`) — the sole source
   of truth. There is no more YAML-manifest-file mechanism at all (removed
   in the same change, chart version >= 0.2.0): `MANIFEST_DIR`, the
   manifests ConfigMap, and `chart/values.yaml`'s `manifests:` block are
   gone. Every backend, ops-managed or user-facing, is registered through
   this API now.
3. Adding or removing a connection takes effect immediately, with no pod
   restart — `POST`/`DELETE` mutate the live in-memory manifest map and
   register/unregister that backend's poll task via
   `mcp_hub.poller.PollerController` (`main.py` wires this once at
   startup). The old design needed a full YAML file + restart to add a
   backend; this doesn't.
4. OAuth connections still work exactly as before at the protocol level
   (`mcp_hub.oauth`/`oauth_setup.py` are unchanged): `GET
   /oauth/{backend}/start` redirects to the provider, `GET
   /oauth/{backend}/callback` completes the exchange and stores tokens in
   mcp-hub's own `oauth_tokens` table. Two router-side details make the
   browser flow work:
   - `POST /connections/{backend}/authorize` (router, not mcp-hub) fetches
     mcp-hub's own `/oauth/{backend}/start` with redirects disabled and
     returns the provider's authorization URL as JSON — the frontend opens
     a popup and navigates it there. A browser `fetch` with `redirect:
     "manual"` can't read a cross-origin redirect's `Location` header
     itself, so this one small translation has to happen server-side; it's
     the one place the router does more than pure proxying for this
     feature.
   - `GET /hub/{tenant}/oauth/{backend}/callback` (router, unauthenticated
     — OAuth providers carry no Clerk bearer token) proxies straight to
     that tenant's mcp-hub `/oauth/{backend}/callback`. The tenant slug is
     validated against `tenantSlugPattern` before use (defense against a
     crafted callback URL), same as before.
5. mcp-hub polls every configured backend independently (one asyncio task
   each, capped concurrency, per-backend backoff) — a slow or dead backend
   never blocks another's availability. `GET /api/connections`/`/api/catalog`
   report each backend's live `state`/`tool_count`.

## Ownership and security boundary

mcp-hub does no Clerk verification of its own — it predates this
platform's auth model and is never reachable except through the shared
router (each tenant's mcp-hub Service is cluster-internal only). The
router's own JWT check is the entire auth boundary for `/connections/`,
unlike `/gateway/`/`/brain/` where the downstream also independently
re-verifies. This is a real, disclosed tradeoff, not an oversight — adding
Clerk-awareness to mcp-hub itself (a general-purpose MCP aggregator, used
outside this platform too) was judged out of scope for this pass.

Each tenant's mcp-hub instance holds only that tenant's own connections and
tokens (`docs/components/multi-tenancy.md`'s "no process/database holds
more than one tenant's credentials" rule) — this was true before this
change and remains true; there is no cross-tenant table anywhere in this
design.

## Rollout

Bump the mcp-hub chart dependency in
`deploy/helm/agent-harness-tenant/Chart.yaml` (>= 0.2.0) and run `helm
dependency update` before deploying. **Not automatic**: a tenant's
previously-static YAML-manifest backends (declared under the old
`mcp-hub.manifests` values key) are not migrated — they need to be
re-registered through `POST /api/connections` after the upgrade. See
`deploy/helm/tenants/abishekk.yaml`'s own TODO for this tenant's specific
list (abrp, exa, finance, grafana, health, maps-engine, notion, trek).

No separate worker, image, Temporal namespace, or database migration is
needed for this feature anymore — it ships entirely inside the mcp-hub
chart bump.

## Verification

In `agent-harness/workflows`, `go test ./internal/router/...` covers the
proxy/authorize routes. In the mcp-hub repo, `python -m pytest` covers the
`connections` table (via testcontainers-backed Postgres) and the
catalog/management API routes (fakes, no DB needed for those). Manual
smoke test: connect GitHub (header token) → confirm it appears in
`GET /api/connections` as `ready` once tools index; connect Notion (OAuth)
→ popup opens the real consent screen → callback completes → same.
Disconnect either and confirm its tools/oauth rows are gone.
