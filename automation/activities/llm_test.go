package activities

import "testing"

func TestLLMUpdate(t *testing.T) {
	if ValidateLLMUpdate(map[string]LLMTier{"fast": {Provider: "openai", Model: "m", BaseURL: "http://x"}}) != nil {
		t.Fatal("valid tier rejected")
	}
	for _, bad := range []map[string]LLMTier{
		{},
		{"huge": {Provider: "openai", Model: "m", BaseURL: "u"}},
		{"fast": {Provider: "ollama", Model: "m", BaseURL: "u"}},
		{"fast": {Provider: "openai", BaseURL: "u"}},
	} {
		if ValidateLLMUpdate(bad) == nil {
			t.Fatalf("accepted %v", bad)
		}
	}

	// blank key must be omitted so Helm keeps the stored one; medium drives agent-brain.
	v := buildLLMValues(map[string]LLMTier{"medium": {Provider: "openai", Model: "local-llm", BaseURL: "http://l"}})
	tier := v["llm"].(map[string]any)["tiers"].(map[string]any)["medium"].(map[string]any)
	if _, has := tier["apiKey"]; has {
		t.Fatal("blank apiKey was sent")
	}
	if v["agent-brain"].(map[string]any)["llm"].(map[string]any)["model"] != "local-llm" {
		t.Fatal("agent-brain retain model not updated")
	}

	views, err := parseLLMTierViews(`{"llm":{"tiers":{"fast":{"provider":"openai","model":"m","baseURL":"u","apiKey":"secret"}}}}`)
	if err != nil || !views["fast"].HasKey || views["fast"].Model != "m" {
		t.Fatalf("views=%v err=%v", views, err)
	}
}
