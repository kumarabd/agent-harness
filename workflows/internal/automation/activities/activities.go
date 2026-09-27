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
//
// 2026-09-26: RegisterTemporalNamespace and RegisterSharedPoolNamespace are
// both gone too — docs/components/multi-tenancy.md's "Resolved: Shared
// Temporal Namespace, Per-Tenant Task Queues". Every tenant now shares ONE
// pre-existing Temporal namespace (TenantTemporalNamespace below) instead of
// getting its own, so there is no namespace left to create per tenant, and
// no shared-pool "temporal.namespaces" list left to append to — onboarding
// a tenant now genuinely never touches the agent-harness-shared release at
// all. SharedChartDir/SharedRelease (only ever used by
// RegisterSharedPoolNamespace) are gone with it; SharedNamespace stays —
// StageTenantSecrets/CleanupStagedSecret still stage the submitted secrets
// in that namespace, an unrelated concern.
type Activities struct {
	TemporalAddress string
	// TenantTemporalNamespace — the one Temporal namespace every tenant's
	// own tenant-worker/Gateway/agent-brain fleet AND the shared
	// loop-worker pool share now ("agents" — named after the Kubernetes
	// namespace every tenant lives in, a cosmetic consistency choice, NOT
	// this automation worker's own separate "system" control-plane
	// namespace, which this worker actually runs on). Written into every
	// generated tenant's temporal.namespace and
	// agent-brain.temporal.namespace.
	TenantTemporalNamespace string

	ChartDir        string // deploy/helm/agent-harness-tenant, baked into this image — see helm.go
	SharedNamespace string // k8s namespace StageTenantSecrets/CleanupStagedSecret stage the submitted secret in

	ClerkIssuer string // the one shared Clerk issuer (Phase 1's single-project migration) — written into every generated tenant's gateway.web.clerkIssuer

	// WebOrigin — agent-web's own public origin (one deployment serves every
	// tenant), written into every generated tenant's gateway.web.
	// allowedOrigins. 2026-09-27: found missing entirely — every self-serve
	// tenant's /web/ws WebSocket upgrade was being silently rejected by
	// gorilla/websocket's own CheckOrigin, since the same-host fallback
	// (workflows/internal/gateway/web/web.go's webSocketOriginAllowed) can
	// never pass once a request has gone through the shared router (r.Host
	// is the router's own hostname by then, never agent-web's).
	WebOrigin string

	GatewayPort    int // matches agent-harness-tenant/values.yaml's gateway.port default (8090) — must match core.Tenant's own defaults on the router side
	AgentBrainPort int // matches that chart's agent-brain subchart default (8080)
}
