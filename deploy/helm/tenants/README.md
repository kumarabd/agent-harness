# Per-tenant value overrides

One file per tenant, named after that tenant's slug (== release name == Kubernetes
namespace == the sanitized form of the tenant's Clerk user id, `tenantid.SlugForSub` —
docs/components/gateway/web.md's "Resolved: Convention-Based Tenant Identity, No
Database"). Each file holds only what's *different* for that tenant from
`../agent-harness-tenant/values.yaml`'s defaults — not a full copy of the chart's
values.

Most tenants never need a hand-written file at all: self-serve onboarding
(docs/components/gateway/web.md's Phase 2/3, `POST /onboard` on the shared
router) provisions a tenant end to end — Kubernetes namespace, Helm release,
LLM/infra secrets — from a form in `agent-web`, with no manual step here.
A file in this directory is for a tenant that needs something the onboarding
form doesn't collect (Discord, non-default LLM tiers beyond the one the form
sets up, resource overrides, ...), or for manual recovery.

Install/upgrade a tenant with its override layered on top of the chart
defaults:

```sh
helm upgrade --install <tenant-slug> deploy/helm/agent-harness-tenant \
  -n <tenant-slug> --create-namespace \
  -f deploy/helm/tenants/<tenant-slug>.yaml
```

`temporal.namespace` does **not** need setting — every tenant shares one
Temporal namespace with the shared pool now (docs/components/multi-tenancy.md's
"Resolved: Shared Temporal Namespace, Per-Tenant Task Queues"), and the
chart's own default is already that shared value. Task queue names are
computed automatically from the release name (`templates/configmap.yaml`),
never a settable value — this is deliberate: it's what keeps one tenant's
activities from ever being dispatched to another tenant's own tenant-worker
now that the namespace isolation boundary is gone.

The one place a per-tenant Temporal value genuinely still has to be set by
hand here: `agent-brain.temporal.retainTaskQueue` (convention:
`<tenant-slug>-memory`) — Helm can't compute this one from the release name
inside a template the way `configmap.yaml` computes the others, because it's
read from the vendored `agent-brain` subchart's own values namespace, which
has to be populated before any template renders (see that value's own
comment in `../agent-harness-tenant/values.yaml` for the full reasoning).
Get this wrong (or leave it at the subchart's own shared default) and this
tenant's retain worker can end up processing another tenant's own retain
workflow — a chart-side `fail` guard catches only the case where it's empty,
not a live collision.

Add whatever else differs for this tenant: `tenantVolume.accessMode`/
`storageClassName` (cluster's available storage classes vary),
`tenantWorker.replicaCount`/`resources`, `postgres.*`, Discord bot tokens
(`gateway.discord.bots`), etc. This is also where a tenant's real
`llm.tiers.<tier>.{model,apiKey,baseURL}` triples belong, if this tenant
needs tiers beyond what onboarding already set up — never in the chart's own
`values.yaml` defaults, which are shared by every tenant install. Every
configured tier owns its own provider identity
(`docs/components/model-registry.md`, 2026-08-28); different tiers can point
at different providers, and there is no cross-tier or shared-default
fallback, so every tier this tenant actually uses must be configured
explicitly (all three fields together).

There is no separate "register this tenant" step anywhere else in the
cluster — no shared-pool Helm value to append to, no registry table to
insert into. The moment this tenant's own namespace/Gateway/agent-brain
exist, the shared router already resolves any request from that tenant's
Clerk user id straight to it (docs/components/gateway/web.md's
"Resolved: Convention-Based Tenant Identity, No Database").
