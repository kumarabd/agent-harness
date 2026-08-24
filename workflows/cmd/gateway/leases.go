package main

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// acquireOrRenewConnectionLease implements docs/components/gateway.md's
// "Resolved: Connection Leasing" — reuses session_filesystem_leases/
// leases.py's exact renewal-based compare-and-swap pattern verbatim, ported
// to Go and to gateway_connection_leases' simpler one-lease-per-platform
// shape (no path dimension the way a session's filesystem lease has — this
// whole Gateway process either holds the one live connection for a given
// platform, or it doesn't; PRIMARY KEY is just `platform`).
//
// Never blocks: returns (false, nil) — not an error — when the lease is
// genuinely held by someone else and hasn't expired. The caller decides
// whether/how to retry (docs/components/gateway.md: "it simply doesn't open
// a live connection this cycle and retries later — no error, no
// coordination protocol beyond the lease itself").
func (s *server) acquireOrRenewConnectionLease(ctx context.Context, platform, holderID string, ttl time.Duration) (bool, error) {
	var gotHolder string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO gateway_connection_leases (platform, holder_id, expires_at)
		VALUES ($1, $2, now() + make_interval(secs => $3))
		ON CONFLICT (platform) DO UPDATE
			SET holder_id = EXCLUDED.holder_id,
			    acquired_at = CASE WHEN gateway_connection_leases.holder_id = EXCLUDED.holder_id
			                       THEN gateway_connection_leases.acquired_at ELSE now() END,
			    expires_at = EXCLUDED.expires_at
			WHERE gateway_connection_leases.holder_id = EXCLUDED.holder_id
			   OR gateway_connection_leases.expires_at < now()
		RETURNING holder_id
	`, platform, holderID, ttl.Seconds()).Scan(&gotHolder)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// The WHERE clause excluded every candidate row — genuinely
			// held by someone else, not expired. Not an error.
			return false, nil
		}
		return false, err
	}
	return gotHolder == holderID, nil
}

// releaseConnectionLease releases a lease this holder currently holds. A
// no-op if it was already reclaimed by someone else after expiring — never
// errors on that, mirroring leases.py's release exactly.
func (s *server) releaseConnectionLease(ctx context.Context, platform, holderID string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM gateway_connection_leases WHERE platform = $1 AND holder_id = $2",
		platform, holderID,
	)
	return err
}
