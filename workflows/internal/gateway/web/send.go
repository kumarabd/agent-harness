package web

import (
	"encoding/json"
	"net/http"

	"agent-harness/workflows/internal/gateway/core"
)

type sendRequest struct {
	Content         string `json:"content"`
	ClientMessageID string `json:"client_message_id"`
	// SessionID/ParentSessionID — gateway.md's "Resolved: Multi-Session
	// Channels", Web's own instance (websession.go). Both empty reproduces
	// today's exact single-session behavior. ParentSessionID is only
	// meaningful the first time a given SessionID is sent (genesis
	// detection in core.Ingest is idempotent, so sending it on every
	// message for an already-existing session is harmless, not required to
	// be precise).
	SessionID       string `json:"session_id"`
	ParentSessionID string `json:"parent_session_id"`
}

type sendResponse struct {
	Status string `json:"status"`
}

// handleSend — docs/components/gateway.md, "Resolved: Inbound Flow" step 2
// only: normalize Web's own raw request body into a MessageEvent
// (inbound.go). Steps 3-6 (session resolution, dedup, SignalWithStart, ack)
// are platform-agnostic and live in core.Ingest — this handler no
// longer needs to know about Postgres or Temporal at all. ChannelID and User
// both resolve to the same Clerk user_id here since Web has no group-chat
// concept (see MessageEvent's own doc comment on why they're still separate
// fields).
func (h *Handler) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" || req.ClientMessageID == "" {
		http.Error(w, "content and client_message_id are required", http.StatusBadRequest)
		return
	}

	userID := userIDFromContext(r.Context())
	discriminator := webDiscriminator(userID, req.SessionID)
	parentSessionKey := ""
	if discriminator != "channel:"+userID {
		// A branched session — resolve its parent (defaults to "main" if
		// the client didn't specify one). Only actually written on genesis
		// (core.Ingest's own ON CONFLICT DO NOTHING), so computing
		// this unconditionally on every message for an existing session is
		// harmless.
		parentSessionKey = core.SessionKeyFor("web", userID, webDiscriminator(userID, req.ParentSessionID))
	}
	event := core.MessageEvent{
		Platform:          "web",
		ChannelID:         userID,
		User:              userID,
		Content:           req.Content,
		PlatformMessageID: req.ClientMessageID,
		Discriminator:     discriminator,
		ParentSessionKey:  parentSessionKey,
	}

	status, err := h.ingestor.Ingest(r.Context(), event)
	if err != nil {
		http.Error(w, "failed to submit message", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, sendResponse{Status: status})
}
