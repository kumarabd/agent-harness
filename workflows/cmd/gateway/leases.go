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
// to Go. Keyed (platform, connectionID) rather than platform alone
// (2026-08-25 correction) — a tenant can run more than one connection of the
// same platform kind (e.g. two Discord bots), each needing its own lease row
// since each is a physically independent live socket. connectionID is
// always a platform-provided stable identity (Discord: the bot's own user
// id via GET /users/@me), resolved by the caller before this is ever called.
//
// Never blocks: returns (false, nil) — not an error — when the lease is
// genuinely held by someone else and hasn't expired. The caller decides
// whether/how to retry (docs/components/gateway.md: "it simply doesn't open
// a live connection this cycle and retries later — no error, no
// coordination protocol beyond the lease itself").
func (s *server) acquireOrRenewConnectionLease(ctx context.Context, platform, connectionID, holderID string, ttl time.Duration) (bool, error) {
	var gotHolder string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO gateway_connection_leases (platform, connection_id, holder_id, expires_at)
		VALUES ($1, $2, $3, now() + make_interval(secs => $4))
		ON CONFLICT (platform, connection_id) DO UPDATE
			SET holder_id = EXCLUDED.holder_id,
			    acquired_at = CASE WHEN gateway_connection_leases.holder_id = EXCLUDED.holder_id
			                       THEN gateway_connection_leases.acquired_at ELSE now() END,
			    expires_at = EXCLUDED.expires_at
			WHERE gateway_connection_leases.holder_id = EXCLUDED.holder_id
			   OR gateway_connection_leases.expires_at < now()
		RETURNING holder_id
	`, platform, connectionID, holderID, ttl.Seconds()).Scan(&gotHolder)
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
func (s *server) releaseConnectionLease(ctx context.Context, platform, connectionID, holderID string) error {
	_, err := s.pool.Exec(ctx,
		"DELETE FROM gateway_connection_leases WHERE platform = $1 AND connection_id = $2 AND holder_id = $3",
		platform, connectionID, holderID,
	)
	return err
}
