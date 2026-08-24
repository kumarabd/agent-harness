package main

import (
	"net/http"
	"time"
)

type sessionSummary struct {
	SessionID       string  `json:"session_id"`
	ParentSessionID *string `json:"parent_session_id"`
	CreatedAt       string  `json:"created_at"`
}

type listSessionsResponse struct {
	Sessions []sessionSummary `json:"sessions"`
}

// handleListSessions — gateway.md's "Resolved: Multi-Session Channels", the
// UI-facing enumeration Web needs (Discord never lists sessions itself; a
// human just replies in whichever channel/thread they're already in).
// Scoped to this authenticated user's own sessions only — WHERE channel_id
// = the real Clerk user_id from the token, never anything client-supplied,
// same isolation every other handler in this file already relies on.
func (s *server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromContext(r.Context())
	ctx := r.Context()

	rows, err := s.pool.Query(ctx,
		"SELECT session_key, parent_session_key, created_at FROM sessions "+
			"WHERE platform = 'web' AND channel_id = $1 ORDER BY created_at",
		userID,
	)
	if err != nil {
		http.Error(w, "failed to list sessions", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var sessions []sessionSummary
	for rows.Next() {
		var sessionKey string
		var parentKey *string
		var createdAt time.Time
		if err := rows.Scan(&sessionKey, &parentKey, &createdAt); err != nil {
			http.Error(w, "failed to list sessions", http.StatusInternalServerError)
			return
		}
		var parentSessionID *string
		if parentKey != nil {
			id := webSessionIDFromKey(userID, *parentKey)
			parentSessionID = &id
		}
		sessions = append(sessions, sessionSummary{
			SessionID:       webSessionIDFromKey(userID, sessionKey),
			ParentSessionID: parentSessionID,
			CreatedAt:       createdAt.Format(time.RFC3339),
		})
	}

	writeJSON(w, http.StatusOK, listSessionsResponse{Sessions: sessions})
}
