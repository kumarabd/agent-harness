package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/types"
)

type respondRequest struct {
	RequestID        string  `json:"request_id"`
	SelectedOptionID *string `json:"selected_option_id"`
	FreeText         *string `json:"free_text"`
	SessionID        string  `json:"session_id"`
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
func (h *Handler) handleRespond(w http.ResponseWriter, r *http.Request) {
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
	sessionKey := core.SessionKeyFor("web", userID, webDiscriminator(userID, req.SessionID))
	ctx := r.Context()

	// core.AnswerUserInput — docs/components/gateway/first-party-plan.md —
	// the ownership check (this request belongs to sessionKey) and the
	// signal-or-idempotent-ack logic below used to live only here; mobile
	// now shares the exact same path.
	payload := types.UserInputResponse{
		RequestID:        req.RequestID,
		SelectedOptionID: req.SelectedOptionID,
		FreeText:         req.FreeText,
	}
	err := core.AnswerUserInput(ctx, h.pool, h.temporal, sessionKey, payload)
	var already *core.AlreadyAnsweredError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, respondResponse{Status: "accepted"})
	case errors.As(err, &already):
		// Already answered/cancelled/expired elsewhere (e.g. the 1-hour
		// timeout, or a stale poll response) — same idempotent ack as
		// /send's own dedup short-circuit, not an error.
		writeJSON(w, http.StatusOK, respondResponse{Status: "already_" + already.Status})
	case errors.Is(err, core.ErrNotOwner):
		http.Error(w, "no such pending request for this session", http.StatusNotFound)
	default:
		http.Error(w, "failed to deliver response", http.StatusInternalServerError)
	}
}
