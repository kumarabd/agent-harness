package core

// Shared session-access handlers — docs/components/gateway/first-party-plan.md.
// "Clients access sessions; clients do not define them." Every client-facing
// platform (web, mobile — never Discord, which is channel- not user-scoped)
// calls these instead of hand-rolling its own list/read/answer SQL, so
// ownership checks and read completeness live in exactly one place. Takes a
// *pgxpool.Pool directly (not an Ingestor) — this is the read/response side,
// Ingestor is the write side; a platform Handler already holds both.

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// ErrNotOwner is returned by AuthorizeSession and AnswerUserInput when the
// caller's userID doesn't own the session/request in question — deliberately
// not distinguished from "doesn't exist" (a locator, not proof of ownership:
// leaking existence to a non-owner is its own information disclosure).
var ErrNotOwner = errors.New("not found or not owned by this user")

// SessionSummary is the client-facing shape ListSessions/AuthorizeSession
// return — platform-inclusive, so "the same authorized session list across
// web and mobile" (the plan's own requirement) falls out of one query rather
// than needing per-platform merging downstream.
type SessionSummary struct {
	SessionKey       string
	ParentSessionKey string // "" if none
	Platform         string
	CreatedAt        time.Time
}

// ListSessions returns every session owned by userID, across every
// client-facing platform, oldest first. Ownership = "channel_id equals this
// user's own identity" — true for web and mobile (both key channel_id on the
// verified Clerk sub), never true for Discord (channel_id there is a Discord
// channel id, not a user id, so those rows never match).
func ListSessions(ctx context.Context, pool *pgxpool.Pool, userID string) ([]SessionSummary, error) {
	rows, err := pool.Query(ctx,
		"SELECT session_key, parent_session_key, platform, created_at FROM sessions "+
			"WHERE channel_id = $1 AND platform IN ('web', 'mobile') ORDER BY created_at",
		userID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var parentKey *string
		if err := rows.Scan(&s.SessionKey, &parentKey, &s.Platform, &s.CreatedAt); err != nil {
			return nil, err
		}
		if parentKey != nil {
			s.ParentSessionKey = *parentKey
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// AuthorizeSession confirms sessionKey both exists and is owned by userID —
// the one check every read/write handler runs before touching a
// client-supplied session_key. A session_key is a locator, never proof of
// ownership on its own.
func AuthorizeSession(ctx context.Context, pool *pgxpool.Pool, userID, sessionKey string) (SessionSummary, error) {
	var s SessionSummary
	var parentKey *string
	err := pool.QueryRow(ctx,
		"SELECT session_key, parent_session_key, platform, created_at FROM sessions "+
			"WHERE session_key = $1 AND channel_id = $2 AND platform IN ('web', 'mobile')",
		sessionKey, userID,
	).Scan(&s.SessionKey, &parentKey, &s.Platform, &s.CreatedAt)
	if err != nil {
		return SessionSummary{}, ErrNotOwner
	}
	if parentKey != nil {
		s.ParentSessionKey = *parentKey
	}
	return s, nil
}

// HistoryMessage is one messages row — every public message, not just the
// first/last per turn (web's old poll behavior truncated this; mobile's
// catchup already read it in full — this is that same completeness, shared).
type HistoryMessage struct {
	Seq            int
	Role           string
	Content        string
	SpeakerID      string
	ClientMsgID    string
	ClientDeviceID string
}

// HistoryTurn is one turn plus its full message list.
type HistoryTurn struct {
	TurnSeq     int
	TurnID      string
	Status      string
	InitiatedBy string
	Messages    []HistoryMessage
}

// defaultHistoryLimit bounds a single ReadHistory call when the caller passes
// limit<=0 — the plan's "bounded pagination without silently skipping older
// history": a caller that actually wants more pages further back re-requests
// with an lower afterTurnSeq bound / walks forward from its own cursor,
// rather than this function ever returning an unbounded result set.
const defaultHistoryLimit = 20

// ReadHistory returns terminal (completed/failed/cancelled) turns for
// sessionKey with turn_seq > afterTurnSeq, oldest first, each with its full
// message list — the single reader both Web polling and the mobile tail call.
// limit<=0 uses defaultHistoryLimit; ALWAYS bounded, never an unbounded scan
// (mobile's old explicit-resume path had no such bound).
func ReadHistory(ctx context.Context, pool *pgxpool.Pool, sessionKey string, afterTurnSeq, limit int) ([]HistoryTurn, error) {
	if limit <= 0 {
		limit = defaultHistoryLimit
	}
	rows, err := pool.Query(ctx,
		"SELECT turn_seq, turn_id, status, COALESCE(initiated_by, 'user') FROM turns "+
			"WHERE parent_id = $1 AND parent_type = 'session' AND turn_seq > $2 "+
			"AND status != 'running' ORDER BY turn_seq LIMIT $3",
		sessionKey, afterTurnSeq, limit,
	)
	if err != nil {
		return nil, err
	}
	var turns []HistoryTurn
	for rows.Next() {
		var t HistoryTurn
		if err := rows.Scan(&t.TurnSeq, &t.TurnID, &t.Status, &t.InitiatedBy); err != nil {
			rows.Close()
			return nil, err
		}
		turns = append(turns, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for i := range turns {
		msgRows, err := pool.Query(ctx,
			"SELECT seq, role, COALESCE(content,''), COALESCE(speaker_id,''), COALESCE(client_msg_id,''), "+
				"COALESCE(client_device_id,'') FROM messages WHERE parent_id = $1 ORDER BY seq",
			turns[i].TurnID,
		)
		if err != nil {
			return nil, err
		}
		for msgRows.Next() {
			var m HistoryMessage
			if err := msgRows.Scan(&m.Seq, &m.Role, &m.Content, &m.SpeakerID, &m.ClientMsgID, &m.ClientDeviceID); err != nil {
				msgRows.Close()
				return nil, err
			}
			turns[i].Messages = append(turns[i].Messages, m)
		}
		msgRows.Close()
		if err := msgRows.Err(); err != nil {
			return nil, err
		}
	}
	return turns, nil
}

// PendingInput is the one pending user_input_request for a session, if any.
type PendingInput struct {
	RequestID     string
	TurnID        string
	Kind          string
	Prompt        string
	Options       []byte // raw jsonb, passed through
	AllowFreeText bool
}

// PendingInputFor returns the most recent pending request for sessionKey, or
// nil if there isn't one — shared by Web's poll response and mobile's
// ask_user tail.
func PendingInputFor(ctx context.Context, pool *pgxpool.Pool, sessionKey string) (*PendingInput, error) {
	var p PendingInput
	err := pool.QueryRow(ctx,
		"SELECT r.request_id, r.turn_id, r.kind, r.prompt, r.options, r.allow_free_text "+
			"FROM user_input_requests r JOIN turns t ON t.turn_id = r.turn_id "+
			"WHERE t.parent_id = $1 AND t.parent_type = 'session' AND r.status = 'pending' "+
			"ORDER BY r.created_at DESC LIMIT 1",
		sessionKey,
	).Scan(&p.RequestID, &p.TurnID, &p.Kind, &p.Prompt, &p.Options, &p.AllowFreeText)
	if err != nil {
		return nil, nil //nolint:nilerr // no pending row is the expected common case, not an error
	}
	return &p, nil
}

// AnswerUserInput validates that requestID's request actually belongs to
// sessionKey (the check web/respond.go always had and mobile never did —
// first-party-plan.md §6) and, only then, signals the response into
// UserInputRequestWorkflow's own execution.
func AnswerUserInput(ctx context.Context, pool *pgxpool.Pool, temporal client.Client, sessionKey string, resp types.UserInputResponse) error {
	var workflowID, status string
	err := pool.QueryRow(ctx,
		"SELECT r.workflow_id, r.status FROM user_input_requests r "+
			"JOIN turns t ON t.turn_id = r.turn_id "+
			"WHERE r.request_id = $1 AND t.parent_id = $2 AND t.parent_type = 'session'",
		resp.RequestID, sessionKey,
	).Scan(&workflowID, &status)
	if err != nil {
		return ErrNotOwner
	}
	if status != "pending" {
		return &AlreadyAnsweredError{Status: status}
	}
	return temporal.SignalWorkflow(ctx, workflowID, "", wf.UserInputResponseSignalName, resp)
}

// CancelActiveTurn validates that sessionKey is owned by userID (the same
// ownership check every other write here runs — a session_key is a locator,
// never proof of ownership) and, only then, signals the session's
// CoordinatorWorkflow to stop whatever turn is currently running. A plain
// SignalWorkflow, never SignalWithStart: a cancel with no active coordinator
// has nothing to do, so it should just silently reach nobody rather than spin
// one up — the coordinator itself further no-ops if there's no active turn to
// forward it into (coordinator.go).
func CancelActiveTurn(ctx context.Context, pool *pgxpool.Pool, temporal client.Client, userID, sessionKey string) error {
	if _, err := AuthorizeSession(ctx, pool, userID, sessionKey); err != nil {
		return err
	}
	return temporal.SignalWorkflow(ctx, sessionKey, "", wf.CancelSignalName, struct{}{})
}

// AlreadyAnsweredError — a request already answered/cancelled/expired
// elsewhere (the 1-hour timeout, a stale poll, another device). Callers
// treat this as an idempotent no-op ack, not a hard error — same as
// web/respond.go always did. Status carries the request's actual terminal
// status so a caller can reconstruct "already_<status>" without a second
// query.
type AlreadyAnsweredError struct{ Status string }

func (e *AlreadyAnsweredError) Error() string { return "request already " + e.Status }
