package activities

import (
	"context"

	"agent-harness/workflows/internal/router/registry"
)

// RegisterTenantInRegistry inserts the tenant_registry row the router
// (workflows/internal/router/core) reads on every proxied request — reuses
// that package's own Registry.Register directly rather than duplicating
// the query, since both binaries must agree on the exact same schema
// (deploy/helm/agent-harness-shared/files/001_tenant_registry.sql). This is
// the step that actually makes the new tenant reachable through
// agent-web/the router; everything before it provisioned infrastructure
// nobody could route to yet.
func (a *Activities) RegisterTenantInRegistry(ctx context.Context, ref PublicRef, orgID string) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		return a.Registry.Register(ctx, registry.Tenant{
			OrgID:          orgID,
			Namespace:      ref.TenantSlug,
			ReleaseName:    ref.TenantSlug,
			GatewayPort:    a.gatewayPort(),
			AgentBrainPort: a.agentBrainPort(),
		})
	})
}
