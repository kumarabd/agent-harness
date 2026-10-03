package core

import (
	"fmt"

	"agent-harness/shared/tenantid"
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
	MapsEnginePort int
}

func (t Tenant) Namespace() string { return t.Slug }

// GatewayBaseURL/AgentBrainBaseURL — bare Service names, no release-name/
// tenant-slug prefix (2026-09-27: each tenant already gets its own
// dedicated Kubernetes namespace — deploy/helm/agent-harness-tenant/
// templates/gateway-service.yaml's componentFullname helper and the
// agent-brain subchart's "memory" fullnameOverride, values.yaml — so the
// namespace alone disambiguates these, a prefix was pure redundancy).
func (t Tenant) GatewayBaseURL() string {
	return fmt.Sprintf("http://gateway.%s.svc.cluster.local:%d", t.Namespace(), t.GatewayPort)
}

func (t Tenant) AgentBrainBaseURL() string {
	return fmt.Sprintf("http://memory-server.%s.svc.cluster.local:%d", t.Namespace(), t.AgentBrainPort)
}

// McpHubBaseURL — 2026-09-26: the router proxies connection-management
// requests directly to each tenant's own mcp-hub instance now (its own
// POST/DELETE /api/connections, GET /api/catalog, /oauth/{backend}/{start,
// callback}) — there is no more separate "connections" service in this
// chart; mcp-hub owns its connections entirely (mcp-hub's own
// src/mcp_hub/store.py ConnectionRecord table). "tools" matches this
// tenant's mcp-hub subchart fullnameOverride (agent-harness-tenant/values.yaml).
func (t Tenant) McpHubBaseURL() string {
	return fmt.Sprintf("http://tools.%s.svc.cluster.local:8000", t.Namespace())
}

// MapsEngineBaseURL — the tenant's own maps-engine (journeys, garage, route planning), a plain Service
// named "maps-engine" in the tenant's namespace. maps-engine does no token verification of its own:
// it trusts headerVerifiedUser and must be reachable only from this router (NetworkPolicy).
func (t Tenant) MapsEngineBaseURL() string {
	return fmt.Sprintf("http://maps-engine.%s.svc.cluster.local:%d", t.Namespace(), t.MapsEnginePort)
}

func TenantForSub(sub string, gatewayPort, agentBrainPort int) Tenant {
	return Tenant{Slug: tenantid.SlugForSub(sub), GatewayPort: gatewayPort, AgentBrainPort: agentBrainPort}
}
