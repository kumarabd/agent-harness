package core

import (
	"fmt"

	"agent-harness/workflows/internal/tenantid"
)

// Tenant identity is now pure convention, not a database lookup — 2026-09-25.
// The Temporal namespace, Kubernetes namespace, and Helm release name for a
// tenant are ALL the same string: a sanitized form of the caller's own Clerk
// user id ("sub" claim). There is no more tenant_registry table, no more
// "register a tenant" step — a tenant's identity is knowable the instant
// they have a valid session, whether or not their infrastructure has
// actually been provisioned yet (that's inferred from whether the proxied
// request actually connects — see proxy.go's ErrorHandler).
type Tenant struct {
	Slug           string
	GatewayPort    int
	AgentBrainPort int
}

// Namespace and ReleaseName are the same value by convention — kept as
// separate methods (rather than collapsing to just Slug everywhere) so call
// sites read the same way they did when these could differ under the old
// registry-backed design.
func (t Tenant) Namespace() string   { return t.Slug }
func (t Tenant) ReleaseName() string { return t.Slug }

// GatewayBaseURL/AgentBrainBaseURL — same cross-namespace Service DNS shape
// as before (deploy/helm/agent-harness-tenant/templates/gateway-service.yaml's
// componentFullname convention, agent-brain subchart's "memory" nameOverride).
func (t Tenant) GatewayBaseURL() string {
	return fmt.Sprintf("http://%s-gateway.%s.svc.cluster.local:%d", t.ReleaseName(), t.Namespace(), t.GatewayPort)
}

func (t Tenant) AgentBrainBaseURL() string {
	return fmt.Sprintf("http://%s-memory-server.%s.svc.cluster.local:%d", t.ReleaseName(), t.Namespace(), t.AgentBrainPort)
}

// McpHubBaseURL — 2026-09-26: the router proxies connection-management
// requests directly to each tenant's own mcp-hub instance now (its own
// POST/DELETE /api/connections, GET /api/catalog, /oauth/{backend}/{start,
// callback}) — there is no more separate "connections" service in this
// chart; mcp-hub owns its connections entirely (mcp-hub's own
// src/mcp_hub/store.py ConnectionRecord table). "tools" matches this
// tenant's mcp-hub subchart nameOverride (agent-harness-tenant/values.yaml).
func (t Tenant) McpHubBaseURL() string {
	return fmt.Sprintf("http://%s-tools.%s.svc.cluster.local:8000", t.ReleaseName(), t.Namespace())
}

func TenantForSub(sub string, gatewayPort, agentBrainPort int) Tenant {
	return Tenant{Slug: tenantid.SlugForSub(sub), GatewayPort: gatewayPort, AgentBrainPort: agentBrainPort}
}
