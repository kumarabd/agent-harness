package web

import (
	"encoding/json"
	"errors"
	"net/http"

	"agent-harness/workflows/internal/gateway/core"
)

type cancelRequest struct {
	SessionID string `json:"session_id"`
}

type cancelResponse struct {
	Status string `json:"status"`
}

// handleCancel — docs/components/gateway/first-party-plan.md's cancel/stop
// primitive. Distinct from /respond: this targets the session's own
// CoordinatorWorkflow (core.CancelActiveTurn), not a specific pending
// request. A cancel with no active turn is a harmless no-op, not an error —
// same idempotent posture as every other handler in this file.
func (h *Handler) handleCancel(w http.ResponseWriter, r *http.Request) {
	var req cancelRequest
	_ = json.NewDecoder(r.Body).Decode(&req) // an empty body means "cancel my main session" — same default as /send's own empty session_id

	userID := userIDFromContext(r.Context())
	sessionKey := core.SessionKeyFor("web", userID, webDiscriminator(userID, req.SessionID))

	if err := core.CancelActiveTurn(r.Context(), h.pool, h.temporal, userID, sessionKey); err != nil {
		if errors.Is(err, core.ErrNotOwner) {
			http.Error(w, "no such session", http.StatusNotFound)
			return
		}
		http.Error(w, "failed to cancel", http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusOK, cancelResponse{Status: "accepted"})
}
