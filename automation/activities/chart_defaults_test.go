package activities

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-harness/shared/tenantid"
)

func chartDir(t *testing.T, llmTiers string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "values.yaml"), []byte("llm:\n  enabled: false\n  tiers:\n"+llmTiers), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOnlyConfiguredChartTiersCountAsDefaults(t *testing.T) {
	tiers, err := chartDefaultTiers(chartDir(t, "    fast: {provider: \"\", model: \"\", apiKey: \"\", baseURL: \"\"}\n    medium: {provider: openai, model: chat-1, apiKey: \"\", baseURL: http://l}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tiers) != 1 || tiers["medium"].Model != "chat-1" || tiers["medium"].BaseURL != "http://l" {
		t.Fatalf("got %+v", tiers)
	}
}

func platformInput() TenantOnboardingInput {
	return TenantOnboardingInput{
		RequesterUserID: "user_abc", TenantSlug: tenantid.SlugForSub("user_abc"),
		PostgresPassword: "p", AgentBrainDBPassword: "p", AgentBrainAPIKey: "p", AgentBrainJWTSecret: "p", McpHubDBPassword: "p",
	}
}

func TestNoTiersIsAcceptedOnlyWhenTheChartShipsADefaultModel(t *testing.T) {
	with := &Activities{ChartDir: chartDir(t, "    medium: {provider: openai, model: chat-1, apiKey: \"\", baseURL: http://l}\n")}
	if err := with.ValidateRequest(context.Background(), platformInput()); err != nil {
		t.Fatal(err)
	}
	without := &Activities{ChartDir: chartDir(t, "    medium: {provider: openai, model: \"\", apiKey: \"\", baseURL: http://l}\n")}
	if err := without.ValidateRequest(context.Background(), platformInput()); err == nil {
		t.Fatal("no requester tiers and no default model must be refused")
	}
}
