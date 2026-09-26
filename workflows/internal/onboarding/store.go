// Package onboarding is the shared Postgres access for self-serve tenant
// onboarding (docs/components/gateway/web.md's Phase 2/3, schema in
// deploy/helm/agent-harness-shared/files/002_tenant_onboarding.sql) — used
// by BOTH the router (workflows/cmd/router, which creates the request row
// and lets the browser poll its progress) and the automation worker
// (workflows/cmd/automation, whose TenantOnboardingWorkflow activities
// record each step's progress). A genuinely shared package rather than
// duplicated queries in each binary, since both sides must agree on the
// exact same schema.
package onboarding

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	StatusPending          = "pending"
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval"
	StatusCompleted        = "completed"
	StatusFailed           = "failed"

	StepStatusRunning = "running"
	StepStatusDone    = "done"
	StepStatusFailed  = "failed"
)

type Request struct {
	RequestID       string
	RequesterUserID string
	TenantSlug      string
	Status          string
	Error           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// JSON tags matter here: workflows/internal/router/core/onboarding.go
// serializes a []Step slice directly (no wrapping struct) as its GET
// /onboard/{request_id} response's "steps" field, and agent-web's
// src/lib/onboarding.ts's OnboardingStep type expects these exact lowercase
// field names.
type Step struct {
	Step      string    `json:"step"`
	Status    string    `json:"status"`
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ErrNotFound mirrors registry.ErrNotFound's shape for the same reason —
// "no such request" is an expected caller-facing state (a stale/garbage
// request_id), not an operational failure.
var ErrNotFound = pgx.ErrNoRows

// CreateRequest inserts a new onboarding request, idempotent on request_id
// (a client-generated id — the router's POST /onboard handler's own
// idempotency key, same pattern as the Gateway's ingested_messages/
// client_message_id). Returns created=false, no error, on a duplicate
// resubmission — the caller (router) treats that as "already accepted",
// not a failure. A concurrent attempt to claim a tenant_slug that already
// has a pending/running/awaiting_approval request fails on the partial
// unique index instead — surfaced as a real error, since that's a genuine
// conflict (two different requests, not a resend of the same one), unlike
// the request_id conflict above.
func (s *Store) CreateRequest(ctx context.Context, req Request) (created bool, err error) {
	tag, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_onboarding_requests (request_id, requester_user_id, tenant_slug, status)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (request_id) DO NOTHING
	`, req.RequestID, req.RequesterUserID, req.TenantSlug, StatusPending)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

func (s *Store) GetRequest(ctx context.Context, requestID string) (Request, error) {
	var r Request
	err := s.pool.QueryRow(ctx, `
		SELECT request_id, requester_user_id, tenant_slug, status, coalesce(error, ''), created_at, updated_at
		FROM tenant_onboarding_requests
		WHERE request_id = $1
	`, requestID).Scan(&r.RequestID, &r.RequesterUserID, &r.TenantSlug, &r.Status, &r.Error, &r.CreatedAt, &r.UpdatedAt)
	return r, err
}

func (s *Store) SetRequestStatus(ctx context.Context, requestID, status, errMsg string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE tenant_onboarding_requests
		SET status = $2, error = NULLIF($3, ''), updated_at = now()
		WHERE request_id = $1
	`, requestID, status, errMsg)
	return err
}

// RecordStep appends one progress row and wakes any live tailer via NOTIFY
// (payload: request_id) — nothing consumes that NOTIFY yet (Phase 3's own
// live-progress UI), harmless to send regardless; a poller (Phase 2's own
// GET /onboard/{request_id}) just re-reads ListSteps directly.
func (s *Store) RecordStep(ctx context.Context, requestID, step, status, message string) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO tenant_onboarding_steps (request_id, step, status, message)
		VALUES ($1, $2, $3, NULLIF($4, ''))
	`, requestID, step, status, message)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `SELECT pg_notify('tenant_onboarding_progress', $1)`, requestID)
	return err
}

func (s *Store) ListSteps(ctx context.Context, requestID string) ([]Step, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT step, status, coalesce(message, ''), updated_at
		FROM tenant_onboarding_steps
		WHERE request_id = $1
		ORDER BY id ASC
	`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var steps []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.Step, &st.Status, &st.Message, &st.UpdatedAt); err != nil {
			return nil, err
		}
		steps = append(steps, st)
	}
	return steps, rows.Err()
}
