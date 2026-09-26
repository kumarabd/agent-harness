package activities

import (
	"context"
	"fmt"
	"regexp"

	"agent-harness/workflows/internal/tenantid"
)

// tenantSlugPattern matches what's actually usable as BOTH a Kubernetes
// namespace name and a Helm release name — RFC 1123 label rules, lowercase
// alphanumeric + hyphen. TenantSlug is computed by the router
// (tenantid.SlugForSub(RequesterUserID)), never user-chosen, so this check
// is a sanity guard against an internal bug producing something unusable,
// not a real-world input-validation concern anymore.
var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,38}[a-z0-9])?$`)

// ValidateRequest is the workflow's first step — fails fast, before
// anything with a real side effect runs, on a malformed request.
func (a *Activities) ValidateRequest(ctx context.Context, in TenantOnboardingInput) error {
	if in.RequesterUserID == "" {
		return fmt.Errorf("requester user id is required")
	}
	if in.TenantSlug != tenantid.SlugForSub(in.RequesterUserID) {
		return fmt.Errorf("tenant slug %q does not match the requester's own derived slug — this should never happen outside a bug", in.TenantSlug)
	}
	if !tenantSlugPattern.MatchString(in.TenantSlug) {
		return fmt.Errorf("tenant slug %q is not a valid Kubernetes namespace / Helm release name", in.TenantSlug)
	}
	if len(in.LLMTiers) == 0 {
		return fmt.Errorf("at least one LLM tier (fast/medium/expert) is required")
	}
	for tier, cfg := range in.LLMTiers {
		if tier != "fast" && tier != "medium" && tier != "expert" {
			return fmt.Errorf("unknown LLM tier %q — must be fast, medium, or expert", tier)
		}
		if cfg.Provider == "" || cfg.Model == "" || cfg.APIKey == "" || cfg.BaseURL == "" {
			return fmt.Errorf("LLM tier %q is missing one of provider/model/apiKey/baseURL — no cross-tier fallback exists (deploy/helm/tenants/README.md)", tier)
		}
		if cfg.Provider != "openai" && cfg.Provider != "anthropic" {
			return fmt.Errorf("LLM tier %q: provider must be \"openai\" or \"anthropic\"", tier)
		}
	}
	for name, v := range map[string]string{
		"postgres password":       in.PostgresPassword,
		"agent-brain db password": in.AgentBrainDBPassword,
		"agent-brain api key":     in.AgentBrainAPIKey,
		"agent-brain jwt secret":  in.AgentBrainJWTSecret,
		"mcp-hub db password":     in.McpHubDBPassword,
		"litellm api key":         in.LiteLLMAPIKey,
	} {
		if v == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	return nil
}
