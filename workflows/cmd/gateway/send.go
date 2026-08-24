package main

import (
	"encoding/json"
	"net/http"
)

type sendRequest struct {
	Content         string `json:"content"`
	ClientMessageID string `json:"client_message_id"`
}

type sendResponse struct {
	Status string `json:"status"`
}

// handleSend — docs/components/gateway.md, "Resolved: Inbound Flow" step 2
// only: normalize Web's own raw request body into a MessageEvent
// (inbound.go). Steps 3-6 (session resolution, dedup, SignalWithStart, ack)
// are platform-agnostic and live in submitMessageEvent — this handler no
// longer needs to know about Postgres or Temporal at all. ChannelID and User
// both resolve to the same Clerk user_id here since Web has no group-chat
// concept (see MessageEvent's own doc comment on why they're still separate
// fields).
func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" || req.ClientMessageID == "" {
		http.Error(w, "content and client_message_id are required", http.StatusBadRequest)
		return
	}

	userID := userIDFromContext(r.Context())
	event := MessageEvent{
		Platform:          "web",
		ChannelID:         userID,
		User:              userID,
		Content:           req.Content,
		PlatformMessageID: req.ClientMessageID,
	}

	status, err := s.submitMessageEvent(r.Context(), event)
	if err != nil {
		http.Error(w, "failed to submit message", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, sendResponse{Status: status})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
