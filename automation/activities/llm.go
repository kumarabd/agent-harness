package activities

import (
	"context"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// LLMUpdateInput is TenantLLMUpdateWorkflow's input — a post-onboarding edit of
// the tenant's llm.tiers. APIKey may be empty per tier, meaning "keep the key
// already stored in the release" (buildLLMValues omits it so Helm's merge
// leaves it alone). The key does land once in Temporal history via this
// input, the same disclosed limitation as StageTenantSecrets.
type LLMUpdateInput struct {
	RequesterUserID string
	TenantSlug      string
	Tiers           map[string]LLMTier
}

func (in LLMUpdateInput) Ref() PublicRef {
	return PublicRef{RequesterUserID: in.RequesterUserID, TenantSlug: in.TenantSlug}
}

// LLMTierView is what GetTenantLLM returns to the UI — never the key itself.
type LLMTierView struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"baseURL"`
	HasKey   bool   `json:"hasKey"`
}

// ValidateLLMUpdate is shared by the router (fail fast with a 400) and HelmUpdateLLM.
func ValidateLLMUpdate(tiers map[string]LLMTier) error {
	if len(tiers) == 0 {
		return fmt.Errorf("at least one LLM tier is required")
	}
	for name, t := range tiers {
		if name != "fast" && name != "medium" && name != "expert" {
			return fmt.Errorf("unknown LLM tier %q — must be fast, medium, or expert", name)
		}
		if t.Provider != "openai" && t.Provider != "anthropic" {
			return fmt.Errorf("LLM tier %q: provider must be \"openai\" or \"anthropic\"", name)
		}
		if t.Model == "" || t.BaseURL == "" {
			return fmt.Errorf("LLM tier %q needs a model and base URL", name)
		}
	}
	return nil
}

// buildLLMValues is the values override for the edit: only llm.tiers (plus
// agent-brain's own retain model when the medium tier changes — it mirrors
// medium, see buildTenantValues). Pure, so it's unit-testable.
func buildLLMValues(tiers map[string]LLMTier) map[string]any {
	out := map[string]any{}
	for name, t := range tiers {
		v := map[string]any{"provider": t.Provider, "model": t.Model, "baseURL": t.BaseURL}
		if t.APIKey != "" {
			v["apiKey"] = t.APIKey
		}
		out[name] = v
	}
	values := map[string]any{"llm": map[string]any{"enabled": true, "tiers": out}}
	if m, ok := tiers["medium"]; ok {
		values["agent-brain"] = map[string]any{"llm": map[string]any{"baseURL": m.BaseURL, "model": m.Model}}
	}
	return values
}

// HelmUpdateLLM applies the edit to the tenant's existing release.
// --reset-then-reuse-values keeps everything onboarding set (secrets, owner,
// ...) while picking up the chart's current defaults; a plain --reuse-values
// would leave newly-added chart values unset.
func (a *Activities) HelmUpdateLLM(ctx context.Context, in LLMUpdateInput) error {
	if err := ValidateLLMUpdate(in.Tiers); err != nil {
		return err
	}
	valuesYAML, err := yaml.Marshal(buildLLMValues(in.Tiers))
	if err != nil {
		return fmt.Errorf("marshal llm values: %w", err)
	}
	if _, err := runCommandStdin(ctx, string(valuesYAML), "helm", "upgrade", in.TenantSlug, a.ChartDir,
		"-n", in.TenantSlug, "--reset-then-reuse-values", "-f", "-"); err != nil {
		return fmt.Errorf("helm upgrade: %w", err)
	}
	return nil
}

// GetTenantLLM reads the tenant's current tiers from the release's own
// user-supplied values, minus the keys.
func (a *Activities) GetTenantLLM(ctx context.Context, ref PublicRef) (map[string]LLMTierView, error) {
	out, err := runCommand(ctx, "helm", "get", "values", ref.TenantSlug, "-n", ref.TenantSlug, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("helm get values: %w", err)
	}
	return parseLLMTierViews(out)
}

func parseLLMTierViews(helmValuesJSON string) (map[string]LLMTierView, error) {
	var v struct {
		LLM struct {
			Tiers map[string]LLMTier `json:"tiers"`
		} `json:"llm"`
	}
	if err := json.Unmarshal([]byte(helmValuesJSON), &v); err != nil {
		return nil, fmt.Errorf("parse helm values: %w", err)
	}
	views := make(map[string]LLMTierView, len(v.LLM.Tiers))
	for name, t := range v.LLM.Tiers {
		views[name] = LLMTierView{Provider: t.Provider, Model: t.Model, BaseURL: t.BaseURL, HasKey: t.APIKey != ""}
	}
	return views, nil
}
