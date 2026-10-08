package core

// Self-serve tenant onboarding (docs/components/gateway/web.md's Phase 2):
// POST /onboard signal-starts automationworkflow.TenantOnboardingWorkflow on
// the "system" namespace's own task queue; GET /onboard reads its live
// progress via a Temporal Query — no database anywhere in this router
// (2026-09-25). The workflow ID itself IS the idempotency key: it's
// deterministic from the caller's own tenant slug
// (tenantid.SlugForSub(sub)), so there is nothing to dedup by hand the way
// a client-generated request_id used to require, and no separate "which
// request is this" path parameter — a signed-in user only ever has one
// onboarding workflow, their own.

import (
	"encoding/json"
	"errors"
	"net/http"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	automationactivities "agent-harness/automation/activities"
	automationworkflow "agent-harness/automation/workflow"
	"agent-harness/shared/tenantid"
)

type onboardLLMTier struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"`
	BaseURL  string `json:"baseURL"`
}

type onboardRequest struct {
	LLMTiers             map[string]onboardLLMTier `json:"llm_tiers"`
	DiscordBotToken      string                    `json:"discord_bot_token"`
	PostgresPassword     string                    `json:"postgres_password"`
	AgentBrainDBPassword string                    `json:"agent_brain_db_password"`
	AgentBrainAPIKey     string                    `json:"agent_brain_api_key"`
	AgentBrainJWTSecret  string                    `json:"agent_brain_jwt_secret"`
	McpHubDBPassword     string                    `json:"mcp_hub_db_password"`
	LiteLLMAPIKey        string                    `json:"litellm_api_key"`
}

func workflowIDForSlug(slug string) string {
	return "tenant-onboard:" + slug
}

// registerOnboarding mounts /onboard on the same mux Handler() builds — see
// that method's own doc comment for why the whole thing is CORS-wrapped.
func (s *Server) registerOnboarding(mux *http.ServeMux) {
	mux.HandleFunc("POST /onboard", s.handleSubmitOnboarding)
	mux.HandleFunc("GET /onboard", s.handleGetOnboarding)
}

func (s *Server) handleSubmitOnboarding(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}

	var req onboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	tenantSlug := tenantid.SlugForSub(sub)
	input := automationactivities.TenantOnboardingInput{
		RequestID:            tenantSlug,
		RequesterUserID:      sub,
		TenantSlug:           tenantSlug,
		DiscordBotToken:      req.DiscordBotToken,
		PostgresPassword:     req.PostgresPassword,
		AgentBrainDBPassword: req.AgentBrainDBPassword,
		AgentBrainAPIKey:     req.AgentBrainAPIKey,
		AgentBrainJWTSecret:  req.AgentBrainJWTSecret,
		McpHubDBPassword:     req.McpHubDBPassword,
		LiteLLMAPIKey:        req.LiteLLMAPIKey,
	}
	if len(req.LLMTiers) > 0 {
		input.LLMTiers = make(map[string]automationactivities.LLMTier, len(req.LLMTiers))
		for name, t := range req.LLMTiers {
			input.LLMTiers[name] = automationactivities.LLMTier{
				Provider: t.Provider, Model: t.Model, APIKey: t.APIKey, BaseURL: t.BaseURL,
			}
		}
	}

	workflowID := workflowIDForSlug(tenantSlug)
	opts := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: s.automationTaskQueue,
		// Retry failed setup, but never rerun successful setup and rotate its credentials.
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY,
	}
	// "start" is never actually handled by the workflow — SignalWithStart's
	// real job here is "start it if it isn't running yet"; if it's already
	// running, this just delivers a harmless unhandled signal to it, same
	// as a resubmit while onboarding is already in progress should do
	// (nothing, since it's already going).
	_, err = s.temporal.SignalWithStartWorkflow(r.Context(), workflowID, "start", nil, opts,
		automationworkflow.TenantOnboardingWorkflow, input)
	var alreadyStarted *serviceerror.WorkflowExecutionAlreadyStarted
	if err != nil && !errors.As(err, &alreadyStarted) {
		writeJSONError(w, http.StatusInternalServerError, "failed to start onboarding workflow")
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
}

func (s *Server) handleGetOnboarding(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}

	workflowID := workflowIDForSlug(tenantid.SlugForSub(sub))
	encoded, err := s.temporal.QueryWorkflow(r.Context(), workflowID, "", automationworkflow.ProgressQuery)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			writeJSONError(w, http.StatusNotFound, "no onboarding request found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to query onboarding progress")
		return
	}

	var progress automationworkflow.Progress
	if err := encoded.Get(&progress); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to decode onboarding progress")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
