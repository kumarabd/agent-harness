package activities

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

type k8sSecretGetResult struct {
	Data map[string]string `json:"data"` // base64-encoded, per Kubernetes' own -o json convention
}

// readStagedSecret re-reads the secret material StageTenantSecrets wrote,
// directly from Kubernetes — never from workflow input, which is the whole
// point (this activity's own input is just PublicRef, no secret fields at
// all; see StageTenantSecrets's doc comment). Everything this returns stays
// local to HelmInstallTenant's own function body: it's used to build a
// values.yaml streamed to `helm upgrade --install` over stdin and is never
// itself returned as this activity's result or logged, so none of it
// re-enters Temporal's recorded history a second time.
func (a *Activities) readStagedSecret(ctx context.Context, requestID string) (map[string]string, error) {
	out, err := runCommand(ctx, "kubectl", "get", "secret", stagedSecretName(requestID), "-n", a.SharedNamespace, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("read staged secret: %w", err)
	}
	var result k8sSecretGetResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return nil, fmt.Errorf("parse staged secret: %w", err)
	}
	decoded := make(map[string]string, len(result.Data))
	for k, v := range result.Data {
		b, err := base64.StdEncoding.DecodeString(v)
		if err != nil {
			return nil, fmt.Errorf("decode staged secret key %q: %w", k, err)
		}
		decoded[k] = string(b)
	}
	return decoded, nil
}

// HelmInstallTenant renders a per-tenant Helm values override, matching
// deploy/helm/tenants/<tenant>.yaml's own real shape field-for-field (see
// that directory's README.md), and runs `helm upgrade --install` against
// the agent-harness-tenant chart baked into this worker's own image
// (a.ChartDir) — the exact command sequence that README already documents
// as the manual process, just piped over stdin instead of a checked-in
// file, and with ownerUserID/clerkIssuer filled in automatically rather
// than hand-copied. Deliberately narrow: storage class/access mode and
// per-backend mcp-hub OAuth manifests are left at chart defaults — see
// docs/components/gateway/web.md's Phase 2 section for why those stayed
// out of a v1 onboarding form.
func (a *Activities) HelmInstallTenant(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		secrets, err := a.readStagedSecret(ctx, ref.RequestID)
		if err != nil {
			return err
		}

		var llmTiers map[string]LLMTier
		if err := json.Unmarshal([]byte(secrets[keyLLMTiersJSON]), &llmTiers); err != nil {
			return fmt.Errorf("decode staged llm tiers: %w", err)
		}
		tiers := map[string]any{}
		for name, tier := range llmTiers {
			tiers[name] = map[string]any{
				"provider": tier.Provider,
				"model":    tier.Model,
				"apiKey":   tier.APIKey,
				"baseURL":  tier.BaseURL,
			}
		}

		var discordBots []map[string]any
		if tok := secrets[keyDiscordBotToken]; tok != "" {
			discordBots = []map[string]any{{"botToken": tok}}
		}

		values := map[string]any{
			"temporal": map[string]any{"namespace": ref.TenantSlug},
			"agentBrain": map[string]any{
				"postgres":    map[string]any{"password": secrets[keyAgentBrainDBPassword]},
				"apiKey":      secrets[keyAgentBrainAPIKey],
				"ownerUserID": ref.RequesterUserID,
			},
			"gateway": map[string]any{
				"enabled": true,
				"web":     map[string]any{"clerkIssuer": a.ClerkIssuer},
				"discord": map[string]any{"bots": discordBots},
			},
			"llm": map[string]any{
				"enabled": true,
				"tiers":   tiers,
			},
			"postgresql": map[string]any{
				"auth": map[string]any{
					"postgresPassword": secrets[keyPostgresPassword],
					"password":         secrets[keyPostgresPassword],
				},
			},
			"mcpHub": map[string]any{
				"postgres": map[string]any{"password": secrets[keyMcpHubDBPassword]},
			},
			"agent-brain": map[string]any{
				"secret": map[string]any{
					"litellmAPIKey": secrets[keyLiteLLMAPIKey],
					"jwtSecret":     secrets[keyAgentBrainJWTSecret],
				},
			},
			"mcp-hub": map[string]any{
				"database": map[string]any{
					"password": secrets[keyMcpHubDBPassword],
					// Same computed form deploy/helm/tenants/README.md documents
					// for hand-written tenant files — release name == k8s
					// namespace == ref.TenantSlug, this repo's own convention.
					"host": fmt.Sprintf("%s-postgresql.%s.svc.cluster.local", ref.TenantSlug, ref.TenantSlug),
				},
				"embedding": map[string]any{"apiKey": secrets[keyLiteLLMAPIKey]},
			},
		}
		valuesYAML, err := yaml.Marshal(values)
		if err != nil {
			return fmt.Errorf("marshal tenant values: %w", err)
		}

		_, err = runCommandStdin(ctx, string(valuesYAML), "helm", "upgrade", "--install", ref.TenantSlug,
			a.ChartDir, "-n", ref.TenantSlug, "--create-namespace", "-f", "-")
		if err != nil {
			return fmt.Errorf("helm upgrade --install: %w", err)
		}
		return nil
	})
}

// RegisterSharedPoolNamespace appends this tenant's namespace to the shared
// agent-harness-shared release's temporal.namespaces and re-runs `helm
// upgrade` — the highest-blast-radius step in this whole workflow (that
// chart's own values.yaml comment: "this rolls the whole shared pool").
// Gated behind an explicit human-approval signal in workflow/onboarding.go
// BEFORE this activity is ever scheduled — this function itself has no
// approval logic, it trusts the workflow already waited.
//
// `helm get values` + append + `helm upgrade --reuse-values` avoids needing
// this worker to know or reconstruct the shared release's own full values
// file (which holds real Clerk/Postgres/LLM config for every OTHER tenant's
// shared-pool wiring — nothing this activity should ever need to see, let
// alone risk overwriting with a partial re-render).
func (a *Activities) RegisterSharedPoolNamespace(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		current, err := runCommand(ctx, "helm", "get", "values", a.SharedRelease, "-n", a.SharedNamespace, "-o", "json")
		if err != nil {
			return fmt.Errorf("read shared release values: %w", err)
		}
		var parsed struct {
			Temporal struct {
				Namespaces []string `json:"namespaces"`
			} `json:"temporal"`
		}
		if err := json.Unmarshal([]byte(current), &parsed); err != nil {
			return fmt.Errorf("parse shared release values: %w", err)
		}
		for _, ns := range parsed.Temporal.Namespaces {
			if ns == ref.TenantSlug {
				return nil // idempotent — a workflow retry landing here again is a no-op
			}
		}
		namespaces := append(parsed.Temporal.Namespaces, ref.TenantSlug)

		override, err := yaml.Marshal(map[string]any{
			"temporal": map[string]any{"namespaces": namespaces},
		})
		if err != nil {
			return fmt.Errorf("marshal namespace override: %w", err)
		}
		_, err = runCommandStdin(ctx, string(override), "helm", "upgrade", a.SharedRelease,
			a.SharedChartDir, "-n", a.SharedNamespace, "--reuse-values", "-f", "-")
		if err != nil {
			return fmt.Errorf("helm upgrade shared release: %w", err)
		}
		return nil
	})
}
