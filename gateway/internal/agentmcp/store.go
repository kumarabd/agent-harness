package agentmcp

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"

	"agent-harness/shared/types"
)

func (a *agent) readTurn(ctx context.Context, owner, sessionKey, turnID, query string) (result, bool, error) {
	out := result{Status: "running"}
	var initial, answer string
	var answerBytes int
	err := a.pool.QueryRow(ctx, `
		SELECT t.status, t.completed_at, COALESCE(u.content, ''),
		       LEFT(COALESCE(a.content, ''), $4), OCTET_LENGTH(COALESCE(a.content, ''))
		FROM turns t JOIN sessions s ON s.session_key = t.parent_id
		LEFT JOIN LATERAL (
		  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'user' ORDER BY seq LIMIT 1
		) u ON true
		LEFT JOIN LATERAL (
		  SELECT content FROM messages WHERE parent_id = t.turn_id AND role = 'assistant' ORDER BY seq DESC LIMIT 1
		) a ON true
		WHERE t.turn_id = $1 AND t.parent_id = $2 AND t.parent_type = 'session'
		  AND s.platform = 'web' AND s.channel_id = $3`,
		turnID, sessionKey, owner, maxAnswerBytes,
	).Scan(&out.Status, &out.CompletedAt, &initial, &answer, &answerBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil
	}
	if err != nil {
		return result{}, false, err
	}
	if initial != query {
		return result{}, true, &publicError{"request_conflict", "request_id already belongs to a different query"}
	}
	if !terminal(out.Status) {
		return out, true, nil
	}
	out.Answer, out.Truncated = clip(answer, maxAnswerBytes), answerBytes > maxAnswerBytes
	// shortcut: six recent top-level tool results, add pagination when a consumer needs full execution history.
	rows, err := a.pool.Query(ctx, `
		SELECT tool_call_id, LEFT(tool_name, 256), status,
		       CASE WHEN OCTET_LENGTH(result::text) <= 1024 THEN result ELSE NULL END,
		       COALESCE(OCTET_LENGTH(result::text) > 1024, false), started_at, completed_at
		FROM tool_calls WHERE parent_id = $1 ORDER BY started_at DESC, tool_call_id DESC LIMIT 7`, turnID)
	if err != nil {
		return result{}, true, err
	}
	defer rows.Close()
	for rows.Next() {
		var s source
		var raw []byte
		if err := rows.Scan(&s.ToolCallID, &s.ToolName, &s.Status, &raw, &s.Truncated, &s.StartedAt, &s.CompletedAt); err != nil {
			return result{}, true, err
		}
		if len(out.Sources) == 6 {
			out.Truncated = true
			break
		}
		s.Result = json.RawMessage(raw)
		out.Truncated = out.Truncated || s.Truncated
		out.Sources = append(out.Sources, s)
	}
	return out, true, rows.Err()
}

type storedInput struct {
	pendingInput
	workflowID string
	status     string
}

func (a *agent) readInput(ctx context.Context, turnID, requestID string) (*storedInput, error) {
	var p storedInput
	var options []byte
	var oversized bool
	// An approval can be parked in a nested subagent, so scope the whole owned turn tree.
	err := a.pool.QueryRow(ctx, `
		WITH RECURSIVE owned_turns AS (
		  SELECT turn_id FROM turns WHERE turn_id = $1
		  UNION
		  SELECT t.turn_id FROM turns t JOIN owned_turns p ON t.parent_id = p.turn_id WHERE t.parent_type = 'turn'
		)
		SELECT r.request_id, r.kind, LEFT(r.prompt, 4000),
		       CASE WHEN OCTET_LENGTH(r.options::text) <= 8192 THEN r.options ELSE '[]'::jsonb END,
		       r.allow_free_text, r.workflow_id, r.status,
		       OCTET_LENGTH(r.prompt) > 4000 OR OCTET_LENGTH(r.options::text) > 8192
		FROM user_input_requests r JOIN owned_turns t ON t.turn_id = r.turn_id
		WHERE ($2 <> '' AND r.request_id = $2) OR ($2 = '' AND r.status = 'pending')
		ORDER BY r.created_at DESC, r.request_id LIMIT 1`, turnID, requestID,
	).Scan(&p.RequestID, &p.Kind, &p.Prompt, &options, &p.AllowFreeText, &p.workflowID, &p.status, &oversized)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if oversized {
		return nil, &publicError{"result_too_large", "Pending input exceeds the MCP preview limit; review in the gateway session or cancel this request"}
	}
	if err := json.Unmarshal(options, &p.Options); err != nil {
		return nil, err
	}
	if p.Options == nil {
		p.Options = []types.UserInputOption{}
	}
	return &p, nil
}
