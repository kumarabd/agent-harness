package web

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"agent-harness/workflows/internal/gateway/core"
)

type polledToolCall struct {
	ToolCallID    string          `json:"tool_call_id"`
	MessageSeq    int             `json:"message_seq"`
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments"`
	IsSubagent    bool            `json:"is_subagent,omitempty"`
	Status        string          `json:"status"`
	Result        json.RawMessage `json:"result,omitempty"`
	PartialOutput string          `json:"partial_output,omitempty"`
	Reason        string          `json:"reason,omitempty"`
	StartedAt     time.Time       `json:"started_at"`
	CompletedAt   *time.Time      `json:"completed_at,omitempty"`
}

type polledTurn struct {
	TurnSeq     int              `json:"turn_seq"`
	TurnID      string           `json:"turn_id"`
	Status      string           `json:"status"`
	UserContent string           `json:"user_content"`
	Content     string           `json:"content"`
	ToolCalls   []polledToolCall `json:"tool_calls,omitempty"`
}

// toolCallsByTurn reads the durable snapshot for every tool call issued
// directly by the given turns (mirrors realtime/conn.go's emitToolCalls
// query — same table, same shape — so a poll-rebuilt view and a live
// WebSocket session agree on what a turn's activity looked like). Scoped to
// top-level turn IDs only, matching emitToolCalls: a subagent's own tool
// calls live under its own turn_id, not its parent's.
func toolCallsByTurn(ctx context.Context, pool *pgxpool.Pool, turnIDs []string) (map[string][]polledToolCall, error) {
	if len(turnIDs) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx,
		"SELECT tc.parent_id, tc.tool_call_id, m.seq, tc.tool_name, tc.arguments, tc.is_subagent, tc.status, "+
			"tc.result, COALESCE(tc.partial_output, ''), COALESCE(tc.reason, ''), tc.started_at, tc.completed_at "+
			"FROM tool_calls tc JOIN messages m ON m.message_id = tc.message_id "+
			"WHERE tc.parent_id = ANY($1) ORDER BY tc.parent_id, m.seq, tc.started_at, tc.tool_call_id",
		turnIDs,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	byTurn := make(map[string][]polledToolCall)
	for rows.Next() {
		var turnID string
		var tc polledToolCall
		var arguments, result []byte
		if err := rows.Scan(
			&turnID, &tc.ToolCallID, &tc.MessageSeq, &tc.ToolName, &arguments,
			&tc.IsSubagent, &tc.Status, &result, &tc.PartialOutput,
			&tc.Reason, &tc.StartedAt, &tc.CompletedAt,
		); err != nil {
			return nil, err
		}
		tc.Arguments = json.RawMessage(arguments)
		if len(result) > 0 {
			tc.Result = json.RawMessage(result)
		}
		byTurn[turnID] = append(byTurn[turnID], tc)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Each skill_io row is a real tool attempt under the confirmed command.
	// Project it into the existing tool card shape for browser history rebuilds.
	skillRows, err := pool.Query(ctx,
		"SELECT tc.parent_id, s.step_id || ':skill:' || s.iteration, m.seq, "+
			"s.request->>'tool', COALESCE(s.request->'arguments', '{}'::jsonb), "+
			"CASE WHEN s.response IS NOT NULL THEN 'ok' "+
			"WHEN tc.result->>'skill_step_terminal' = 'true' THEN tc.status ELSE 'pending' END, "+
			"s.response, COALESCE(CASE WHEN s.response IS NULL AND tc.result->>'skill_step_terminal' = 'true' THEN tc.reason END, ''), "+
			"s.created_at, COALESCE(s.completed_at, CASE WHEN tc.result->>'skill_step_terminal' = 'true' THEN tc.completed_at END) "+
			"FROM skill_io s JOIN tool_calls tc ON tc.tool_call_id = s.step_id "+
			"JOIN messages m ON m.message_id = tc.message_id "+
			"WHERE tc.parent_id = ANY($1) AND s.request ? 'tool' "+
			"ORDER BY tc.parent_id, m.seq, s.iteration",
		turnIDs,
	)
	if err != nil {
		return nil, err
	}
	defer skillRows.Close()
	for skillRows.Next() {
		var turnID string
		var tc polledToolCall
		var arguments, result []byte
		if err := skillRows.Scan(&turnID, &tc.ToolCallID, &tc.MessageSeq, &tc.ToolName,
			&arguments, &tc.Status, &result, &tc.Reason, &tc.StartedAt, &tc.CompletedAt); err != nil {
			return nil, err
		}
		tc.Arguments = json.RawMessage(arguments)
		if len(result) > 0 {
			tc.Result = json.RawMessage(result)
		}
		byTurn[turnID] = append(byTurn[turnID], tc)
	}
	return byTurn, skillRows.Err()
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
func (h *Handler) handlePoll(w http.ResponseWriter, r *http.Request) {
	userID := userIDFromContext(r.Context())
	sessionKey := core.SessionKeyFor(h.platform, userID, sessionDiscriminator(userID, r.URL.Query().Get("session_id")))
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
	rows, err := h.pool.Query(ctx,
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
			"WHERE t.parent_id = $1 AND t.parent_type = 'session' AND (t.turn_seq > $2 OR EXISTS ("+
			"SELECT 1 FROM tool_calls tc JOIN skill_io s ON s.step_id = tc.tool_call_id "+
			"WHERE tc.parent_id = t.turn_id AND s.request ? 'tool' AND ("+
			"(tc.result->>'skill_step_terminal' IS DISTINCT FROM 'true' AND tc.started_at > now() - interval '15 minutes') OR "+
			"tc.completed_at > now() - interval '5 minutes'))) "+
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

	// Attach each turn's durable tool-call activity — closes the gap where a
	// browser refresh rebuilds chat from this endpoint alone and previously
	// lost every tool card (they only ever existed as live WebSocket
	// tool_call frames, nothing in the poll shape carried them back).
	turnIDs := make([]string, len(turns))
	for i, t := range turns {
		turnIDs[i] = t.TurnID
	}
	byTurn, err := toolCallsByTurn(ctx, h.pool, turnIDs)
	if err != nil {
		http.Error(w, "failed to read tool calls", http.StatusInternalServerError)
		return
	}
	for i := range turns {
		turns[i].ToolCalls = byTurn[turns[i].TurnID]
	}

	// docs/components/user-input.md — a pending approval/decision request
	// for any turn under this session, if one exists. Direct join, no new
	// mechanism: the same reasoning that made turn delivery collapse to a
	// Postgres read applies here too.
	var pending *pendingInput
	row := h.pool.QueryRow(ctx,
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
