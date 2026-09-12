package mobile

// Cross-replica presence — docs/components/gateway/mobile.md's "Deferred:
// Cross-replica presence". Backed by a real table (migration 033), not a
// Temporal signal/Query on CoordinatorWorkflow: a mobile client tails its
// session via the NOTIFY trigger entirely independent of the coordinator,
// which idles out and exits with no active turn — exactly the state a
// presence check most needs to work during. Presence is connection-layer,
// ephemeral, non-deterministic state (the same category the NOTIFY
// mechanism itself is), not durable business state Temporal workflows model.
//
// Self-healing by construction: last_seen_at is heartbeat-updated on the
// existing WS ping cycle (conn.go's pingEvery, no new timer), and every read
// filters on staleness — a crashed connection (no clean disconnect) ages out
// on its own, no reaper/cleanup job needed.

import (
	"context"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// replicaID identifies which gateway pod owns a presence row — purely for
// observability (which replica is a device connected to), never read back
// to decide anything. The pod's own hostname (== pod name in k8s) is already
// unique and already what shows up in `kubectl get pods`, so it needs no
// separate identity scheme.
var replicaID = func() string {
	h, err := os.Hostname()
	if err != nil {
		return "unknown"
	}
	return h
}()

// presenceStaleAfterSeconds — a row older than this reads as "gone"
// regardless of whether it was ever explicitly removed. ~3.6x pingEvery
// (25s): survives one or two missed heartbeats without a false negative,
// still tight enough to reflect a real disconnect within under two minutes.
const presenceStaleAfterSeconds = 90

// presenceUpsert records (or refreshes) this connection as live — called on
// connect and on every WS ping heartbeat. ON CONFLICT means a reconnecting
// device just refreshes its existing row rather than erroring or
// duplicating. Best-effort: a failed write here just means this heartbeat
// didn't land, exactly like a missed NOTIFY — the next one self-heals it.
func presenceUpsert(ctx context.Context, pool *pgxpool.Pool, sessionKey, deviceID string) {
	_, _ = pool.Exec(ctx,
		"INSERT INTO mobile_presence (session_key, device_id, replica_id, connected_at, last_seen_at) "+
			"VALUES ($1, $2, $3, now(), now()) "+
			"ON CONFLICT (session_key, device_id) DO UPDATE SET replica_id = $3, last_seen_at = now()",
		sessionKey, deviceID, replicaID,
	)
}

// presenceRemove deletes this connection's row on a clean disconnect — the
// common-case immediate cleanup. A crash (no clean disconnect) skips this
// and instead ages out via presenceStaleAfterSeconds — never left stale
// forever, no reaper needed either way.
func presenceRemove(ctx context.Context, pool *pgxpool.Pool, sessionKey, deviceID string) {
	_, _ = pool.Exec(ctx,
		"DELETE FROM mobile_presence WHERE session_key = $1 AND device_id = $2",
		sessionKey, deviceID,
	)
}

// Present reports whether any device is currently connected, on ANY
// replica, for sessionKey — the real, authoritative, cross-replica answer.
// Exported for future callers (a Go consumer, or documentation for a Python
// activity's equivalent direct SQL — presence has no cross-language RPC,
// every consumer just queries mobile_presence itself, same as any other
// table in this system).
func Present(ctx context.Context, pool *pgxpool.Pool, sessionKey string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM mobile_presence WHERE session_key = $1 "+
			"AND last_seen_at > now() - interval '90 seconds')", // presenceStaleAfterSeconds
		sessionKey,
	).Scan(&exists)
	return exists, err
}
