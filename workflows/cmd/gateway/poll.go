package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

type polledTurn struct {
	TurnSeq     int    `json:"turn_seq"`
	TurnID      string `json:"turn_id"`
	Status      string `json:"status"`
	UserContent string `json:"user_content"`
	Content     string `json:"content"`
}

type pendingInput struct {
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	Prompt        string          `json:"prompt"`
	Options       json.RawMessage `json:"options"`
	AllowFreeText bool            `json:"allow_free_text"`
}

type pollResponse struct {
	Turns        []polledTurn  `json:"turns"`
	PendingInput *pendingInput `json:"pending_input"`
}

// handlePoll — docs/components/gateway/web.md, "Real simplification this
// unlocks": delivery collapses for a polling client. No DeliverActivity, no
// embedded Temporal worker, no task-queue routing — just a direct read of
// what ModelCall/InsertMessage already wrote. Also closes
// docs/components/user-input.md's "mid-turn interim delivery" open item for
// Web specifically: a pending approval/decision request is just another
// thing this same read checks for, no separate delivery mechanism needed.
func (s *server) handlePoll(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromContext(r.Context())
	sessionKey := sessionKeyFor("web", userID)
	ctx := r.Context()

	sinceTurnSeq := 0
	if raw := r.URL.Query().Get("since_turn_seq"); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil {
			sinceTurnSeq = v
		}
	}

	// Only completed/failed/cancelled turns — a still-'running' turn has
	// nothing to show yet (this harness has no token-level streaming,
	// gateway/web.md's own "Separately, worth being explicit about" note —
	// a turn's content only exists once it's actually done). Both the
	// inbound user content and the final assistant reply are read back here
	// — a chat page reconstructing full history on load (not just live
	// delivery) needs both sides of the turn, not just the reply; the
	// user's own content was never otherwise returned to the client that
	// sent it (turn.go's InsertMessage call writes it, nothing reads it
	// back before this).
	rows, err := s.pool.Query(ctx,
		"SELECT t.turn_seq, t.turn_id, t.status, COALESCE(u.content, ''), COALESCE(a.content, '') "+
			"FROM turns t "+
			"LEFT JOIN LATERAL ("+
			"  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'user' "+
			"  ORDER BY seq ASC LIMIT 1"+
			") u ON true "+
			"LEFT JOIN LATERAL ("+
			"  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'assistant' "+
			"  ORDER BY seq DESC LIMIT 1"+
			") a ON true "+
			"WHERE t.parent_id = $1 AND t.parent_type = 'session' AND t.turn_seq > $2 "+
			"AND t.status != 'running' ORDER BY t.turn_seq",
		sessionKey, sinceTurnSeq,
	)
	if err != nil {
		http.Error(w, "failed to read turns", http.StatusInternalServerError)
		return
	}
	var turns []polledTurn
	for rows.Next() {
		var t polledTurn
		if err := rows.Scan(&t.TurnSeq, &t.TurnID, &t.Status, &t.UserContent, &t.Content); err != nil {
			rows.Close()
			http.Error(w, "failed to read turns", http.StatusInternalServerError)
			return
		}
		turns = append(turns, t)
	}
	rows.Close()

	// docs/components/user-input.md — a pending approval/decision request
	// for any turn under this session, if one exists. Direct join, no new
	// mechanism: the same reasoning that made turn delivery collapse to a
	// Postgres read applies here too.
	var pending *pendingInput
	row := s.pool.QueryRow(ctx,
		"SELECT r.request_id, r.kind, r.prompt, r.options, r.allow_free_text "+
			"FROM user_input_requests r JOIN turns t ON t.turn_id = r.turn_id "+
			"WHERE t.parent_id = $1 AND t.parent_type = 'session' AND r.status = 'pending' "+
			"ORDER BY r.created_at DESC LIMIT 1",
		sessionKey,
	)
	var p pendingInput
	switch err := row.Scan(&p.RequestID, &p.Kind, &p.Prompt, &p.Options, &p.AllowFreeText); err {
	case nil:
		pending = &p
	default:
		// No rows is the expected common case, not an error — anything
		// else genuinely is, but best-effort here matches this handler's
		// overall tolerance (a missing pending-input check shouldn't fail
		// the whole poll response when turns were read successfully).
	}

	writeJSON(w, http.StatusOK, pollResponse{Turns: turns, PendingInput: pending})
}
