package activities

import (
	"context"
	"encoding/json"
	"fmt"

	"gopkg.in/yaml.v3"
)

// stagedSecretKeys — the literal keys HelmInstallTenant reads back via
// `kubectl get secret ... -o json`.
const (
	keyPostgresPassword     = "postgres-password"
	keyAgentBrainDBPassword = "agentbrain-db-password"
	keyAgentBrainAPIKey     = "agentbrain-api-key"
	keyAgentBrainJWTSecret  = "agentbrain-jwt-secret"
	keyMcpHubDBPassword     = "mcphub-db-password"
	keyLiteLLMAPIKey        = "litellm-api-key"
	keyDiscordBotToken      = "discord-bot-token"
	keyLLMTiersJSON         = "llm-tiers-json"
)

func stagedSecretName(requestID string) string {
	return "tenant-onboard-" + requestID
}

// k8sSecretManifest is deliberately hand-built here (a small, direct YAML
// struct) rather than via `kubectl create secret --from-literal=...`: CLI
// flags land in the child process's own argv, visible to any other process
// on the same host via /proc/<pid>/cmdline for the (brief) window it runs
// — a real, avoidable exposure for genuinely secret values, distinct from
// (and in addition to) the Temporal-history concern this activity's own
// doc comment already covers. `stringData` (plain text, not `data`'s
// pre-base64 requirement) lets the API server do the encoding, piped to
// `kubectl apply -f -` over stdin — never a command-line argument, and
// exec.go's own heartbeat message never includes stdin content.
type k8sSecretManifest struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   k8sObjectMeta     `yaml:"metadata"`
	Type       string            `yaml:"type"`
	StringData map[string]string `yaml:"stringData"`
}

type k8sObjectMeta struct {
	Name      string `yaml:"name"`
	Namespace string `yaml:"namespace"`
}

// StageTenantSecrets is the ONE activity whose input carries the full,
// secret-bearing TenantOnboardingInput — Temporal records every activity
// input verbatim in the workflow's own event history, so this is the one
// place that inevitably still happens (a real, disclosed, bounded
// limitation — full elimination would need a Temporal Codec Server, out of
// scope for this pass, docs/components/gateway/web.md's Phase 2 section
// flags it explicitly). Writes every secret field into a short-lived
// Kubernetes Secret in this worker's own namespace; every activity AFTER
// this one takes only PublicRef and re-reads secret material directly from
// that Secret via kubectl (never from workflow input again) — see
// helm.go's HelmInstallTenant and CleanupStagedSecret below, the one
// activity that deletes it.
func (a *Activities) StageTenantSecrets(ctx context.Context, in TenantOnboardingInput) error {
	return runStep(ctx, a.Store, in.RequestID, func(ctx context.Context) error {
		tiersJSON, err := json.Marshal(in.LLMTiers)
		if err != nil {
			return fmt.Errorf("marshal llm tiers: %w", err)
		}

		manifest := k8sSecretManifest{
			APIVersion: "v1",
			Kind:       "Secret",
			Metadata:   k8sObjectMeta{Name: stagedSecretName(in.RequestID), Namespace: a.SharedNamespace},
			Type:       "Opaque",
			StringData: map[string]string{
				keyPostgresPassword:     in.PostgresPassword,
				keyAgentBrainDBPassword: in.AgentBrainDBPassword,
				keyAgentBrainAPIKey:     in.AgentBrainAPIKey,
				keyAgentBrainJWTSecret:  in.AgentBrainJWTSecret,
				keyMcpHubDBPassword:     in.McpHubDBPassword,
				keyLiteLLMAPIKey:        in.LiteLLMAPIKey,
				keyDiscordBotToken:      in.DiscordBotToken,
				keyLLMTiersJSON:         string(tiersJSON),
			},
		}
		manifestYAML, err := yaml.Marshal(manifest)
		if err != nil {
			return fmt.Errorf("marshal secret manifest: %w", err)
		}
		if _, err := runCommandStdin(ctx, string(manifestYAML), "kubectl", "apply", "-f", "-"); err != nil {
			return fmt.Errorf("apply staged secret: %w", err)
		}
		return nil
	})
}

// CleanupStagedSecret deletes the Secret StageTenantSecrets created —
// called unconditionally at the end of the workflow (success or failure
// path, see workflow/onboarding.go), so a half-failed onboarding never
// leaves real tenant secrets sitting in this worker's own namespace
// indefinitely.
func (a *Activities) CleanupStagedSecret(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		_, err := runCommand(ctx, "kubectl", "delete", "secret", stagedSecretName(ref.RequestID),
			"-n", a.SharedNamespace, "--ignore-not-found")
		return err
	})
}
