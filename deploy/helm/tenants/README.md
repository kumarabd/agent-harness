# Per-tenant value overrides

One file per tenant, named after that tenant's Temporal namespace (e.g.
`agents.yaml` for the `agents` namespace). Each file holds only what's
*different* for that tenant from `../agent-harness-tenant/values.yaml`'s
defaults — not a full copy of the chart's values.

Install/upgrade a tenant with its override layered on top of the chart
defaults:

```sh
helm upgrade --install <release> deploy/helm/agent-harness-tenant \
  -n <k8s-namespace> --create-namespace \
  -f deploy/helm/tenants/<tenant>.yaml
```

At minimum, every tenant file sets `temporal.namespace` — that's the actual
per-tenant isolation boundary (docs/components/multi-tenancy.md) and the
chart's own default (`"default"`) is a local-dev-only value, not meant to be
used for a real tenant. Add whatever else differs for that tenant:
`tenantVolume.accessMode`/`storageClassName` (cluster's available storage
classes vary), `tenantWorker.replicaCount`/`resources`, `postgres.*`, etc.
This is also where a tenant's real `llm.tiers.<tier>.{model,apiKey,baseURL}`
triples belong, if this tenant needs real LLM calls — never in the chart's
own `values.yaml` defaults, which are shared by every tenant install.
Every configured tier owns its own provider identity
(`docs/components/model-registry.md`, 2026-08-28); different tiers can
point at different providers, and there is no cross-tier or shared-default
fallback, so every tier this tenant actually uses must be configured
explicitly (all three fields together).

Don't forget the other half of onboarding, which lives outside this
directory: the tenant's Temporal namespace itself must already exist
(`infra/temporal`'s chart), and its namespace must be added to
`../agent-harness-shared/values.yaml`'s `temporal.namespaces` list — that's
what actually gets its turns scheduled, independent of this chart.
