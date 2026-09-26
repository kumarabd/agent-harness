// Package activities implements TenantOnboardingWorkflow's real, Go-native
// provisioning steps (docs/components/gateway/web.md's Phase 2) — creating
// the tenant's Temporal namespace and Kubernetes namespace, installing the
// agent-harness-tenant Helm release, registering it with the shared pool
// and the router's own tenant_registry, and creating its Clerk
// Organization. Each activity records its own progress via
// workflows/internal/onboarding (Store.RecordStep) — see runStep in
// progress.go for the shared start/success/failure wrapper every activity
// in this package uses.
package activities

// LLMTier mirrors deploy/helm/agent-harness-tenant/values.yaml's
// llm.tiers.<tier> shape exactly (provider/model/apiKey/baseURL) — no
// cross-tier fallback exists in that chart, so every tier the tenant
// actually wants must be fully specified here, same discipline.
type LLMTier struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"`
	BaseURL  string `json:"baseURL"`
}

// TenantOnboardingInput is TenantOnboardingWorkflow's own input — built by
// the router's POST /onboard handler from the submitted form
// (docs/components/gateway/web.md's Phase 3 owns the actual form; this
// struct is the stable contract between that HTTP layer and this
// workflow). Field-for-field, everything here maps onto a real, required
// value in deploy/helm/agent-harness-tenant/values.yaml — see
// docs/components/gateway/web.md's Phase 2 section for which fields were
// deliberately left out of a v1 onboarding form (storage class/access
// mode, per-backend mcp-hub OAuth manifests) and why.
type TenantOnboardingInput struct {
	RequestID       string
	RequesterUserID string // Clerk user_id of the signed-in requester — becomes agentBrain.ownerUserID
	TenantSlug      string // k8s namespace AND Helm release name (this repo's own convention, e.g. "abishekk")

	// At least one tier required; keys are "fast"/"medium"/"expert" — same
	// three tiers deploy/helm/agent-harness-tenant/values.yaml's own
	// llm.tiers block defines, no others.
	LLMTiers map[string]LLMTier

	DiscordBotToken string // optional — empty means Web-only, matches gateway.discord.bots: []

	// Infra secrets — real required values (deploy/helm/agent-harness-tenant/
	// values.yaml's own comments name each one "do not commit a real value
	// here, set per tenant"). The Phase 3 form pre-fills these with a
	// client-generated random default the user can accept or edit
	// (docs/components/gateway/web.md); this workflow just takes whatever
	// value it's given, generation is a browser-side UX concern, not this
	// workflow's.
	PostgresPassword     string // postgresql.auth.{postgresPassword,password}
	AgentBrainDBPassword string // agentBrain.postgres.password
	AgentBrainAPIKey     string // agentBrain.apiKey
	AgentBrainJWTSecret  string // agent-brain.secret.jwtSecret
	McpHubDBPassword     string // mcpHub.postgres.password / mcp-hub.database.password (must match, see values.yaml's own comment)
	// One shared key reused across agent-brain.secret.litellmAPIKey,
	// mcp-hub.embedding.apiKey, and (if voice is ever enabled later)
	// gateway.speechAPIKey — the exact same real-world pattern
	// deploy/helm/tenants/abishekk.yaml already uses (one litellm-service
	// key, several consumers), not a new convention invented here.
	LiteLLMAPIKey string
}

// PublicRef is the non-secret subset of TenantOnboardingInput
// (RequestID/RequesterUserID/TenantSlug — none of them sensitive) passed to
// every activity that doesn't need the real secret fields. Temporal records
// every activity's input verbatim in the workflow's own event history, so
// this is what actually bounds where the real secret payload appears in
// that history to a single activity call (StageTenantSecrets) instead of
// once per activity — see that activity's own doc comment for the full
// reasoning and its known limitation.
type PublicRef struct {
	RequestID       string
	RequesterUserID string
	TenantSlug      string
}

func (in TenantOnboardingInput) Ref() PublicRef {
	return PublicRef{RequestID: in.RequestID, RequesterUserID: in.RequesterUserID, TenantSlug: in.TenantSlug}
}

// TenantOnboardingResult is the workflow's own return value — mostly
// diagnostic (GetWorkflow().Get() isn't actually how progress is consumed,
// see workflows/internal/router/core's onboarding handler, but Temporal
// still records this in history either way).
type TenantOnboardingResult struct {
	TenantSlug string
	Namespace  string
}
