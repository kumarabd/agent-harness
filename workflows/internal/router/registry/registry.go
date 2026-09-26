// Package registry looks up which tenant a Clerk organization belongs to —
// the router's own Postgres table (tenant_registry, deploy/helm/
// agent-harness-shared/files/001_tenant_registry.sql), never a values.yaml
// list, specifically so registering a new tenant (docs/components/
// gateway/web.md's onboarding flow) never requires redeploying this chart.
package registry

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Tenant is one row of tenant_registry — enough for the router to compute
// that tenant's Gateway/agent-brain Service DNS names by itself (cross-
// namespace, since the router and every tenant's own services live in
// different Kubernetes namespaces).
type Tenant struct {
	OrgID          string
	Namespace      string
	ReleaseName    string
	GatewayPort    int
	AgentBrainPort int
}

// GatewayBaseURL is this tenant's own per-tenant Gateway (workflows/cmd/
// gateway, deploy/helm/agent-harness-tenant/templates/gateway-service.yaml —
// Service name is always "<release>-gateway", componentFullname's own
// convention), reachable cross-namespace by its full cluster-DNS name.
func (t Tenant) GatewayBaseURL() string {
	return fmt.Sprintf("http://%s-gateway.%s.svc.cluster.local:%d", t.ReleaseName, t.Namespace, t.GatewayPort)
}

// AgentBrainBaseURL is this tenant's own per-tenant agent-brain explorer API
// (agent-brain subchart's nameOverride "memory" — Service name is always
// "<release>-memory-server", the exact same computation agent-web's own
// deployment template used to do in-namespace before this move).
func (t Tenant) AgentBrainBaseURL() string {
	return fmt.Sprintf("http://%s-memory-server.%s.svc.cluster.local:%d", t.ReleaseName, t.Namespace, t.AgentBrainPort)
}

type Registry struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Registry {
	return &Registry{pool: pool}
}

// ErrNotFound is returned when no tenant is registered for the given org —
// the expected, valid state for a signed-up-but-not-yet-onboarded user, not
// an operational failure.
var ErrNotFound = fmt.Errorf("no tenant registered for this organization")

func (r *Registry) Lookup(ctx context.Context, orgID string) (Tenant, error) {
	var t Tenant
	err := r.pool.QueryRow(ctx, `
		SELECT org_id, namespace, release_name, gateway_port, agent_brain_port
		FROM tenant_registry
		WHERE org_id = $1
	`, orgID).Scan(&t.OrgID, &t.Namespace, &t.ReleaseName, &t.GatewayPort, &t.AgentBrainPort)
	if err != nil {
		if err == pgx.ErrNoRows {
			return Tenant{}, ErrNotFound
		}
		return Tenant{}, err
	}
	return t, nil
}

// Register upserts a tenant's registry row — called by Phase 2's
// TenantOnboardingWorkflow (RegisterTenantInRegistryActivity) once
// provisioning succeeds. Exported here (not left as a raw SQL string in the
// automation worker) so both binaries agree on the one schema this package
// owns.
func (r *Registry) Register(ctx context.Context, t Tenant) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO tenant_registry (org_id, namespace, release_name, gateway_port, agent_brain_port)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (org_id) DO UPDATE SET
			namespace = EXCLUDED.namespace,
			release_name = EXCLUDED.release_name,
			gateway_port = EXCLUDED.gateway_port,
			agent_brain_port = EXCLUDED.agent_brain_port
	`, t.OrgID, t.Namespace, t.ReleaseName, t.GatewayPort, t.AgentBrainPort)
	return err
}
