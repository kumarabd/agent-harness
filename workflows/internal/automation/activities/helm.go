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

// buildTenantValues renders a per-tenant Helm values override, matching
// deploy/helm/tenants/<tenant>.yaml's own real shape field-for-field (see
// that directory's README.md) — with ownerUserID/clerkIssuer filled in
// automatically rather than hand-copied. Deliberately narrow: storage
// class/access mode and per-backend mcp-hub OAuth manifests are left at
// chart defaults — see docs/components/gateway/web.md's Phase 2 section for
// why those stayed out of a v1 onboarding form. Pure and side-effect-free
// (no exec, no I/O) specifically so a values-schema gap like the
// agent-brain.llm/.temporal one below can be caught by a unit test instead
// of only by manually cross-referencing every field against a real tenant's
// working values.yaml — which is how both were actually found.
func buildTenantValues(ref PublicRef, clerkIssuer, temporalAddress, tenantTemporalNamespace string, secrets map[string]string) (map[string]any, error) {
	var llmTiers map[string]LLMTier
	if err := json.Unmarshal([]byte(secrets[keyLLMTiersJSON]), &llmTiers); err != nil {
		return nil, fmt.Errorf("decode staged llm tiers: %w", err)
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

	// agent-brain's own retain/reflect worker (charts/agent-brain/templates/
	// _helpers.tpl's temporalWorkerEnv: LLM_BASE_URL/LLM_MODEL, TEMPORAL_
	// ADDRESS/TEMPORAL_NAMESPACE/TEMPORAL_RETAIN_TASK_QUEUE) reads these from
	// its own top-level llm.baseURL/model and temporal.address/namespace/
	// retainTaskQueue — NOT nested under a "mining" key (agent-harness-tenant/
	// values.yaml's own default had a stale agentBrain.mining.llm.* path
	// pointing at nothing the subchart actually consumes; fixed alongside
	// this). None of these have a chart-side fail guard or safe default
	// (they render as empty strings otherwise), so leaving them unset here
	// silently broke retain/reflect for every self-serve onboarded tenant —
	// found by comparing against deploy/helm/tenants/abishekk.yaml, which
	// sets them by hand.
	//
	// retainTaskQueue MUST be tenant-prefixed (2026-09-26, docs/components/
	// multi-tenancy.md's "Resolved: Shared Temporal Namespace, Per-Tenant
	// Task Queues") — every tenant's own agent-brain instance is a separate
	// pod, but they all now share ONE Temporal namespace, so the subchart's
	// own fixed default ("agent-brain-retain") would have every tenant's
	// retain worker polling the SAME queue: any tenant's retain workflow
	// could be picked up by another tenant's own agent-brain pod, which
	// holds THAT tenant's own Postgres/API credentials. This can't be fixed
	// chart-side (agent-brain's own template reads it from ITS OWN values
	// namespace, populated before any template renders, so it can't be
	// computed from .Release.Name the way this chart's OWN configmap.yaml
	// computes the tenant-worker/Gateway queue names) — it has to be an
	// explicit value here, same as namespace/address already are.
	//
	// "medium" mirrors abishekk.yaml's own convention ("mining[/retain] is
	// treated as a medium-tier consumer") — falls back to whichever tier
	// actually exists if the requester didn't configure a medium one
	// (ValidateRequest only guarantees at least one tier, not which).
	retainTier, hasMedium := llmTiers["medium"]
	if !hasMedium {
		// llmTiers is guaranteed non-empty by ValidateRequest, so this always
		// finds one — "medium" just isn't guaranteed to be the one present.
		for _, name := range []string{"fast", "expert"} {
			if tier, present := llmTiers[name]; present {
				retainTier = tier
				break
			}
		}
	}

	// agentBrainSecret/mcpHubValues: litellmAPIKey/embedding.apiKey are only
	// set when the requester brought their own external embedding endpoint.
	// Left empty (the common case — "use the platform's shared model"), the
	// key is omitted from this values override entirely rather than sent as
	// "", so Helm falls through to the chart's own default
	// (deploy/helm/agent-harness-tenant/values.yaml's agentBrain.secret.
	// litellmAPIKey / mcpHub.embedding.apiKey) instead of overwriting it with
	// an empty string. This cluster's litellm-service has exactly one real
	// key (its master key, already the chart default) — there is no
	// per-tenant virtual-key system to generate a new one from, so a
	// self-serve tenant that doesn't bring their own must get that one.
	agentBrainSecret := map[string]any{
		"jwtSecret": secrets[keyAgentBrainJWTSecret],
	}
	mcpHubOverrides := map[string]any{
		"database": map[string]any{
			"password": secrets[keyMcpHubDBPassword],
			// Same computed form deploy/helm/tenants/README.md documents
			// for hand-written tenant files — release name == k8s
			// namespace == ref.TenantSlug, this repo's own convention.
			"host": fmt.Sprintf("%s-postgresql.%s.svc.cluster.local", ref.TenantSlug, ref.TenantSlug),
		},
	}
	if key := secrets[keyLiteLLMAPIKey]; key != "" {
		agentBrainSecret["litellmAPIKey"] = key
		mcpHubOverrides["embedding"] = map[string]any{"apiKey": key}
	}

	return map[string]any{
		"temporal": map[string]any{"namespace": tenantTemporalNamespace},
		"agentBrain": map[string]any{
			"postgres":    map[string]any{"password": secrets[keyAgentBrainDBPassword]},
			"apiKey":      secrets[keyAgentBrainAPIKey],
			"ownerUserID": ref.RequesterUserID,
		},
		"gateway": map[string]any{
			"enabled": true,
			"web":     map[string]any{"clerkIssuer": clerkIssuer},
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
			"secret": agentBrainSecret,
			"llm": map[string]any{
				"baseURL": retainTier.BaseURL,
				"model":   retainTier.Model,
			},
			"temporal": map[string]any{
				"address":         temporalAddress,
				"namespace":       tenantTemporalNamespace,
				"retainTaskQueue": ref.TenantSlug + "-memory",
			},
		},
		"mcp-hub": mcpHubOverrides,
	}, nil
}

// HelmInstallTenant runs `helm upgrade --install` against the
// agent-harness-tenant chart baked into this worker's own image
// (a.ChartDir) — the exact command sequence deploy/helm/tenants/README.md
// already documents as the manual process, just piped over stdin instead
// of a checked-in file. See buildTenantValues for what actually goes into
// that values override.
func (a *Activities) HelmInstallTenant(ctx context.Context, ref PublicRef) error {
	secrets, err := a.readStagedSecret(ctx, ref.RequestID)
	if err != nil {
		return err
	}

	values, err := buildTenantValues(ref, a.ClerkIssuer, a.TemporalAddress, a.TenantTemporalNamespace, secrets)
	if err != nil {
		return err
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
}

