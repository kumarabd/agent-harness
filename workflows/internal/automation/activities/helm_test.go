package activities

import (
	"encoding/json"
	"testing"
)

func stagedSecretsFor(t *testing.T, tiers map[string]LLMTier) map[string]string {
	t.Helper()
	tiersJSON, err := json.Marshal(tiers)
	if err != nil {
		t.Fatalf("marshal tiers: %v", err)
	}
	return map[string]string{
		keyPostgresPassword:     "pg-pw",
		keyAgentBrainDBPassword: "ab-db-pw",
		keyAgentBrainAPIKey:     "ab-api-key",
		keyAgentBrainJWTSecret:  "ab-jwt",
		keyMcpHubDBPassword:     "mcphub-db-pw",
		keyLiteLLMAPIKey:        "litellm-key",
		keyDiscordBotToken:      "",
		keyLLMTiersJSON:         string(tiersJSON),
	}
}

func atPath(t *testing.T, values map[string]any, path ...string) any {
	t.Helper()
	var cur any = values
	for _, p := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not a map (got %T)", path, p, cur)
		}
		cur, ok = m[p]
		if !ok {
			t.Fatalf("path %v: missing key %q", path, p)
		}
	}
	return cur
}

func TestBuildTenantValuesSetsAgentBrainLLMFromMediumTier(t *testing.T) {
	ref := PublicRef{TenantSlug: "acme", RequesterUserID: "user_123"}
	secrets := stagedSecretsFor(t, map[string]LLMTier{
		"fast":   {Provider: "openai", Model: "fast-model", BaseURL: "https://fast.example"},
		"medium": {Provider: "openai", Model: "medium-model", BaseURL: "https://medium.example"},
	})

	values, err := buildTenantValues(ref, "https://issuer.example", "temporal:7233", secrets)
	if err != nil {
		t.Fatalf("buildTenantValues: %v", err)
	}

	if got := atPath(t, values, "agent-brain", "llm", "baseURL"); got != "https://medium.example" {
		t.Errorf("agent-brain.llm.baseURL = %v, want the medium tier's baseURL", got)
	}
	if got := atPath(t, values, "agent-brain", "llm", "model"); got != "medium-model" {
		t.Errorf("agent-brain.llm.model = %v, want the medium tier's model", got)
	}
}

func TestBuildTenantValuesFallsBackWhenNoMediumTier(t *testing.T) {
	ref := PublicRef{TenantSlug: "acme"}
	secrets := stagedSecretsFor(t, map[string]LLMTier{
		"expert": {Provider: "openai", Model: "expert-model", BaseURL: "https://expert.example"},
	})

	values, err := buildTenantValues(ref, "https://issuer.example", "temporal:7233", secrets)
	if err != nil {
		t.Fatalf("buildTenantValues: %v", err)
	}

	if got := atPath(t, values, "agent-brain", "llm", "model"); got != "expert-model" {
		t.Errorf("agent-brain.llm.model = %v, want the only configured tier's model when no medium tier exists", got)
	}
}

func TestBuildTenantValuesSetsAgentBrainTemporalToTenantNamespace(t *testing.T) {
	ref := PublicRef{TenantSlug: "acme"}
	secrets := stagedSecretsFor(t, map[string]LLMTier{"medium": {Model: "m", BaseURL: "https://x"}})

	values, err := buildTenantValues(ref, "https://issuer.example", "temporal-frontend.core.svc.cluster.local:7233", secrets)
	if err != nil {
		t.Fatalf("buildTenantValues: %v", err)
	}

	if got := atPath(t, values, "agent-brain", "temporal", "address"); got != "temporal-frontend.core.svc.cluster.local:7233" {
		t.Errorf("agent-brain.temporal.address = %v, want the shared Temporal address", got)
	}
	if got := atPath(t, values, "agent-brain", "temporal", "namespace"); got != "acme" {
		t.Errorf("agent-brain.temporal.namespace = %v, want the tenant's own slug", got)
	}
	// Must match the top-level temporal.namespace this tenant's own worker
	// uses — agent-brain's retain worker has no isolation of its own.
	if got := atPath(t, values, "temporal", "namespace"); got != "acme" {
		t.Errorf("temporal.namespace = %v, want %q", got, "acme")
	}
}

func TestBuildTenantValuesPassesThroughClerkIssuer(t *testing.T) {
	ref := PublicRef{TenantSlug: "acme"}
	secrets := stagedSecretsFor(t, map[string]LLMTier{"medium": {Model: "m", BaseURL: "https://x"}})

	values, err := buildTenantValues(ref, "https://issuer.example", "temporal:7233", secrets)
	if err != nil {
		t.Fatalf("buildTenantValues: %v", err)
	}

	if got := atPath(t, values, "gateway", "web", "clerkIssuer"); got != "https://issuer.example" {
		t.Errorf("gateway.web.clerkIssuer = %v, want %q", got, "https://issuer.example")
	}
}

func TestBuildTenantValuesRejectsMalformedStagedTiers(t *testing.T) {
	ref := PublicRef{TenantSlug: "acme"}
	secrets := stagedSecretsFor(t, nil)
	secrets[keyLLMTiersJSON] = "not json"

	if _, err := buildTenantValues(ref, "https://issuer.example", "temporal:7233", secrets); err == nil {
		t.Fatal("expected an error decoding malformed staged llm tiers, got nil")
	}
}
