package main

import (
	"encoding/json"
	"net/http"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

type respondRequest struct {
	RequestID        string  `json:"request_id"`
	SelectedOptionID *string `json:"selected_option_id"`
	FreeText         *string `json:"free_text"`
}

type respondResponse struct {
	Status string `json:"status"`
}

// handleRespond answers a pending user_input_requests row surfaced by
// handlePoll's pending_input field (docs/components/user-input.md). Unlike
// /send, this never goes through SignalWithStartWorkflow — the target is
// UserInputRequestWorkflow's OWN execution (a child workflow, not the
// session's CoordinatorWorkflow), addressed by the workflow_id that row
// already recorded when RequestUserInput created it, so a plain
// SignalWorkflow is enough.
func (s *server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var req respondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.RequestID == "" {
		http.Error(w, "request_id is required", http.StatusBadRequest)
		return
	}
	if req.SelectedOptionID == nil && req.FreeText == nil {
		http.Error(w, "selected_option_id or free_text is required", http.StatusBadRequest)
		return
	}

	userID := userIDFromContext(r.Context())
	sessionKey := sessionKeyFor("web", userID)
	ctx := r.Context()

	// Resolve workflow_id AND confirm this request actually belongs to the
	// caller's own session — same join handlePoll's own pending_input query
	// uses, so a caller can never answer a request surfaced under a
	// different user's session_key.
	var workflowID, status string
	err := s.pool.QueryRow(ctx,
		"SELECT r.workflow_id, r.status FROM user_input_requests r "+
			"JOIN turns t ON t.turn_id = r.turn_id "+
			"WHERE r.request_id = $1 AND t.parent_id = $2 AND t.parent_type = 'session'",
		req.RequestID, sessionKey,
	).Scan(&workflowID, &status)
	if err != nil {
		http.Error(w, "no such pending request for this session", http.StatusNotFound)
		return
	}
	if status != "pending" {
		// Already answered/cancelled/expired elsewhere (e.g. the 1-hour
		// timeout, or a stale poll response) — same "already_accepted"-style
		// idempotent ack as /send's own dedup short-circuit, not an error.
		writeJSON(w, http.StatusOK, respondResponse{Status: "already_" + status})
		return
	}

	payload := types.UserInputResponse{
		RequestID:        req.RequestID,
		SelectedOptionID: req.SelectedOptionID,
		FreeText:         req.FreeText,
	}
	if err := s.temporal.SignalWorkflow(ctx, workflowID, "", wf.UserInputResponseSignalName, payload); err != nil {
		http.Error(w, "failed to deliver response", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, respondResponse{Status: "accepted"})
}
