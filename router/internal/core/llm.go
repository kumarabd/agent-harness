package core

// Post-onboarding edits of a tenant's LLM tiers: GET /llm reads the current
// config (no keys), PUT /llm starts automationworkflow.TenantLLMUpdateWorkflow,
// GET /llm/update reads its progress. Same auth + identity convention as
// /onboard — a caller only ever reaches their own tenant (SlugForSub(sub)).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	automationactivities "agent-harness/automation/activities"
	automationworkflow "agent-harness/automation/workflow"
	"agent-harness/shared/tenantid"
)

func (s *Server) registerLLM(mux *http.ServeMux) {
	mux.HandleFunc("GET /llm", s.handleGetLLM)
	mux.HandleFunc("PUT /llm", s.handlePutLLM)
	mux.HandleFunc("GET /llm/update", s.handleGetLLMUpdate)
}

func llmUpdateWorkflowID(slug string) string { return "tenant-llm:" + slug }

func (s *Server) handleGetLLM(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	slug := tenantid.SlugForSub(sub)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	run, err := s.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID:        "tenant-llm-read:" + slug + ":" + strconv.FormatInt(time.Now().UnixNano(), 36),
		TaskQueue: s.automationTaskQueue,
	}, automationworkflow.TenantLLMReadWorkflow, automationactivities.PublicRef{RequesterUserID: sub, TenantSlug: slug})
	var tiers map[string]automationactivities.LLMTierView
	if err == nil {
		err = run.Get(ctx, &tiers)
	}
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "could not read this workspace's model settings")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"llm_tiers": tiers})
}

func (s *Server) handlePutLLM(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	var req struct {
		LLMTiers map[string]onboardLLMTier `json:"llm_tiers"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	slug := tenantid.SlugForSub(sub)
	in := automationactivities.LLMUpdateInput{
		RequesterUserID: sub, TenantSlug: slug,
		Tiers: make(map[string]automationactivities.LLMTier, len(req.LLMTiers)),
	}
	for name, t := range req.LLMTiers {
		in.Tiers[name] = automationactivities.LLMTier{Provider: t.Provider, Model: t.Model, APIKey: t.APIKey, BaseURL: t.BaseURL}
	}
	// ValidateLLMUpdate runs in the activity too; failing fast here gives the form a 400, not a failed workflow.
	if err := automationactivities.ValidateLLMUpdate(in.Tiers); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	_, err = s.temporal.ExecuteWorkflow(r.Context(), client.StartWorkflowOptions{
		ID:                       llmUpdateWorkflowID(slug),
		TaskQueue:                s.automationTaskQueue,
		WorkflowIDReusePolicy:    enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
		WorkflowIDConflictPolicy: enumspb.WORKFLOW_ID_CONFLICT_POLICY_FAIL,
	}, automationworkflow.TenantLLMUpdateWorkflow, in)
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	switch {
	case errors.As(err, &started):
		writeJSONError(w, http.StatusConflict, "a model change is already in progress")
	case err != nil:
		writeJSONError(w, http.StatusInternalServerError, "failed to start the model update")
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

func (s *Server) handleGetLLMUpdate(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	encoded, err := s.temporal.QueryWorkflow(r.Context(), llmUpdateWorkflowID(tenantid.SlugForSub(sub)), "", automationworkflow.ProgressQuery)
	if err != nil {
		var notFound *serviceerror.NotFound
		if errors.As(err, &notFound) {
			writeJSONError(w, http.StatusNotFound, "no model update found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to query model update progress")
		return
	}
	var progress automationworkflow.Progress
	if err := encoded.Get(&progress); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to decode model update progress")
		return
	}
	writeJSON(w, http.StatusOK, progress)
}
