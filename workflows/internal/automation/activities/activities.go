package activities

// Activities holds every dependency this package's activity methods need —
// a single struct registered once with the worker (workflows/cmd/automation/
// main.go), matching the "composition root builds shared infra, wires it
// into one place" shape every other cmd/ binary in this repo already uses.
//
// 2026-09-25: no Postgres anywhere in this package anymore. Progress used to
// be activity-recorded rows in a tenant_onboarding_steps table
// (workflows/internal/onboarding.Store, since deleted); now the WORKFLOW
// itself (workflow/onboarding.go) tracks progress in local state and
// exposes it via a Temporal Query handler — activities are plain functions
// again, no shared runStep wrapper needed. Tenant registration (writing an
// org_id -> namespace/release row) is also gone: tenant identity is pure
// convention now (workflows/internal/router/core/tenant.go), so there is
// nothing left to register.
type Activities struct {
	TemporalAddress        string // dialed fresh per RegisterTemporalNamespace call — see that file's own comment on why a NamespaceClient isn't reused
	NamespaceRetentionDays int

	ChartDir        string // deploy/helm/agent-harness-tenant, baked into this image — see helm.go
	SharedChartDir  string // deploy/helm/agent-harness-shared, baked into this image
	SharedRelease   string // the shared chart's own release name (e.g. "harness")
	SharedNamespace string // k8s namespace the shared release lives in

	ClerkIssuer string // the one shared Clerk issuer (Phase 1's single-project migration) — written into every generated tenant's gateway.web.clerkIssuer

	GatewayPort    int // matches agent-harness-tenant/values.yaml's gateway.port default (8090) — must match core.Tenant's own defaults on the router side
	AgentBrainPort int // matches that chart's agent-brain subchart default (8080)
}
