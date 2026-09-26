package core

// Self-serve tenant onboarding (docs/components/gateway/web.md's Phase 2):
// POST /onboard submits a request and signal-starts
// automationworkflow.TenantOnboardingWorkflow on the "system" namespace's
// own task queue; GET /onboard/{request_id} lets the browser poll its
// progress. Same SignalWithStartWorkflow + Postgres-idempotency-dedup shape
// workflows/internal/gateway/core/inbound.go's own Ingest already uses for
// chat messages — deliberately mirrored rather than inventing a new
// submission pattern.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	automationactivities "agent-harness/workflows/internal/automation/activities"
	automationworkflow "agent-harness/workflows/internal/automation/workflow"
	"agent-harness/workflows/internal/onboarding"
)

type onboardLLMTier struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	APIKey   string `json:"apiKey"`
	BaseURL  string `json:"baseURL"`
}

type onboardRequest struct {
	RequestID            string                    `json:"request_id"`
	TenantSlug           string                    `json:"tenant_slug"`
	LLMTiers             map[string]onboardLLMTier `json:"llm_tiers"`
	DiscordBotToken      string                    `json:"discord_bot_token"`
	PostgresPassword     string                    `json:"postgres_password"`
	AgentBrainDBPassword string                    `json:"agent_brain_db_password"`
	AgentBrainAPIKey     string                    `json:"agent_brain_api_key"`
	AgentBrainJWTSecret  string                    `json:"agent_brain_jwt_secret"`
	McpHubDBPassword     string                    `json:"mcp_hub_db_password"`
	LiteLLMAPIKey        string                    `json:"litellm_api_key"`
}

// registerOnboarding mounts /onboard on the same mux Handler() builds — see
// that method's own doc comment for why the whole thing is CORS-wrapped.
func (s *Server) registerOnboarding(mux *http.ServeMux) {
	mux.HandleFunc("POST /onboard", s.handleSubmitOnboarding)
	mux.HandleFunc("GET /onboard/{request_id}", s.handleGetOnboarding)
}

func (s *Server) handleSubmitOnboarding(w http.ResponseWriter, r *http.Request) {
	userID, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}

	var req onboardRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RequestID == "" || req.TenantSlug == "" {
		writeJSONError(w, http.StatusBadRequest, "request_id and tenant_slug are required")
		return
	}

	created, err := s.onboarding.CreateRequest(r.Context(), onboarding.Request{
		RequestID:       req.RequestID,
		RequesterUserID: userID,
		TenantSlug:      req.TenantSlug,
	})
	if err != nil {
		// The partial unique index on tenant_slug (deploy/helm/agent-harness-shared/
		// files/002_tenant_onboarding.sql) is the likely real cause here — a
		// different request already claimed this slug while still pending/
		// running/awaiting_approval — surfaced as 409, not 500, since it's a
		// genuine client-correctable conflict, not a server failure.
		if strings.Contains(err.Error(), "tenant_onboarding_requests_slug_active_idx") {
			writeJSONError(w, http.StatusConflict, "tenant_slug already has an onboarding request in progress")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to record onboarding request")
		return
	}
	if !created {
		// Same request_id resubmitted — already accepted, not an error
		// (identical to core.Ingest's own ON CONFLICT DO NOTHING handling).
		writeJSON(w, http.StatusOK, map[string]string{"request_id": req.RequestID, "status": "already_accepted"})
		return
	}

	input := automationactivities.TenantOnboardingInput{
		RequestID:            req.RequestID,
		RequesterUserID:      userID,
		TenantSlug:           req.TenantSlug,
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

	workflowID := "tenant-onboard:" + req.TenantSlug
	opts := client.StartWorkflowOptions{
		ID:                    workflowID,
		TaskQueue:             s.automationTaskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}
	_, err = s.temporal.SignalWithStartWorkflow(r.Context(), workflowID, "start", nil, opts,
		automationworkflow.TenantOnboardingWorkflow, input)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to start onboarding workflow: %v", err))
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]string{"request_id": req.RequestID, "status": "accepted"})
}

func (s *Server) handleGetOnboarding(w http.ResponseWriter, r *http.Request) {
	userID, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}

	requestID := r.PathValue("request_id")
	req, err := s.onboarding.GetRequest(r.Context(), requestID)
	if err != nil {
		writeJSONError(w, http.StatusNotFound, "no such onboarding request")
		return
	}
	if req.RequesterUserID != userID {
		// Same "never another user's" discipline
		// workflows/internal/gateway/core/access.go already applies to
		// session ownership — a request_id is guessable/enumerable, so
		// ownership must be checked, not just existence.
		writeJSONError(w, http.StatusNotFound, "no such onboarding request")
		return
	}

	steps, err := s.onboarding.ListSteps(r.Context(), requestID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to list onboarding steps")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"request_id":  req.RequestID,
		"tenant_slug": req.TenantSlug,
		"status":      req.Status,
		"error":       req.Error,
		"steps":       steps,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
