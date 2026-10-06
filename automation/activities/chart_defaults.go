package activities

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// chartDefaultTiers returns the tiers the tenant chart ships with (deploy/helm/agent-harness-tenant/values.yaml, llm.tiers) that
// are actually configured, meaning they name a model. They are what a workspace gets when the requester brings no models of
// their own: the worker then omits llm.tiers from the tenant's values and the chart's own defaults apply. A requester's tiers
// override only the tiers they set, since Helm merges values maps tier by tier.
func chartDefaultTiers(chartDir string) (map[string]LLMTier, error) {
	raw, err := os.ReadFile(filepath.Join(chartDir, "values.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read tenant chart defaults: %w", err)
	}
	var d struct {
		LLM struct {
			Tiers map[string]struct {
				Provider string `yaml:"provider"`
				Model    string `yaml:"model"`
				APIKey   string `yaml:"apiKey"`
				BaseURL  string `yaml:"baseURL"`
			} `yaml:"tiers"`
		} `yaml:"llm"`
	}
	if err := yaml.Unmarshal(raw, &d); err != nil {
		return nil, fmt.Errorf("parse tenant chart defaults: %w", err)
	}
	out := map[string]LLMTier{}
	for name, t := range d.LLM.Tiers {
		if t.Model != "" {
			out[name] = LLMTier{Provider: t.Provider, Model: t.Model, APIKey: t.APIKey, BaseURL: t.BaseURL}
		}
	}
	return out, nil
}
