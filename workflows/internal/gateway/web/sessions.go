package web

import (
	"net/http"
	"time"
)

type sessionSummary struct {
	SessionID       string  `json:"session_id"`
	ParentSessionID *string `json:"parent_session_id"`
	CreatedAt       string  `json:"created_at"`
	UpdatedAt       string  `json:"updated_at"`
	Title           string  `json:"title"`
	Preview         string  `json:"preview"`
	IsRunning       bool    `json:"is_running"`
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
func (h *Handler) handleListSessions(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromContext(r.Context())
	ctx := r.Context()

	rows, err := h.pool.Query(ctx,
		"SELECT s.session_key, s.parent_session_key, s.created_at, "+
			"GREATEST(s.last_active_at, COALESCE(latest_message.created_at, s.last_active_at)) AS updated_at, "+
			"LEFT(REGEXP_REPLACE(COALESCE(NULLIF(BTRIM(s.title), ''), NULLIF(BTRIM(first_user.content), ''), 'New conversation'), '[[:space:]]+', ' ', 'g'), 80), "+
			"LEFT(REGEXP_REPLACE(COALESCE(NULLIF(BTRIM(latest_message.content), ''), ''), '[[:space:]]+', ' ', 'g'), 140), "+
			"EXISTS (SELECT 1 FROM turns running WHERE running.parent_id = s.session_key "+
			"AND running.parent_type = 'session' AND running.status = 'running') "+
			"FROM sessions s "+
			"LEFT JOIN LATERAL ("+
			"  SELECT m.content FROM turns t JOIN messages m ON m.parent_id = t.turn_id "+
			"  WHERE t.parent_id = s.session_key AND t.parent_type = 'session' AND m.role = 'user' "+
			"  ORDER BY t.turn_seq ASC, m.seq ASC LIMIT 1"+
			") first_user ON true "+
			"LEFT JOIN LATERAL ("+
			"  SELECT m.content, m.created_at FROM turns t JOIN messages m ON m.parent_id = t.turn_id "+
			"  WHERE t.parent_id = s.session_key AND t.parent_type = 'session' "+
			"  AND m.role IN ('user', 'assistant') AND NULLIF(BTRIM(m.content), '') IS NOT NULL "+
			"  ORDER BY t.turn_seq DESC, m.seq DESC LIMIT 1"+
			") latest_message ON true "+
			"WHERE s.platform = 'web' AND s.channel_id = $1 "+
			"ORDER BY CASE WHEN s.parent_session_key IS NULL THEN 0 ELSE 1 END, updated_at DESC, s.created_at DESC",
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
		var createdAt, updatedAt time.Time
		var title, preview string
		var isRunning bool
		if err := rows.Scan(&sessionKey, &parentKey, &createdAt, &updatedAt, &title, &preview, &isRunning); err != nil {
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
			UpdatedAt:       updatedAt.Format(time.RFC3339),
			Title:           title,
			Preview:         preview,
			IsRunning:       isRunning,
		})
	}

	writeJSON(w, http.StatusOK, listSessionsResponse{Sessions: sessions})
}
