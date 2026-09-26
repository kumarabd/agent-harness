package activities

import (
	"context"
	"fmt"
	"regexp"
)

// tenantSlugPattern matches what's actually usable as BOTH a Kubernetes
// namespace name and a Helm release name (this repo's own convention, e.g.
// "abishekk" — deploy/helm/tenants/README.md) — RFC 1123 label rules,
// lowercase alphanumeric + hyphen, 3-40 chars (well under Kubernetes'
// real 63-char DNS label limit, leaving room for this chart's own
// "<release>-<component>" suffixing, e.g. "<slug>-gateway").
var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,38}[a-z0-9])?$`)

// ValidateRequest is the workflow's first step — fails fast, before
// anything with a real side effect runs, on a malformed request. Does NOT
// check tenant_slug uniqueness against the live registry/cluster (the
// router's own POST /onboard handler already enforces that via the
// tenant_onboarding_requests partial unique index before this workflow is
// even started — see workflows/internal/router/core's onboarding handler);
// re-checking here would just be a slower, redundant version of the same
// guarantee.
func (a *Activities) ValidateRequest(ctx context.Context, in TenantOnboardingInput) error {
	return runStep(ctx, a.Store, in.RequestID, func(ctx context.Context) error {
		if !tenantSlugPattern.MatchString(in.TenantSlug) {
			return fmt.Errorf("tenant slug %q is not a valid Kubernetes namespace / Helm release name", in.TenantSlug)
		}
		if in.RequesterUserID == "" {
			return fmt.Errorf("requester user id is required")
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
	})
}
