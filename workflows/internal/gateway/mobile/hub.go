package mobile

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// hub is one per gateway replica. It holds a single `LISTEN mobile_stream`
// Postgres connection and a registry of the WebSocket connections this replica
// currently serves, keyed by session_key. On each notification it wakes every
// local connection for that session; the connection re-reads its own tail.
//
// NOTIFY is treated purely as a wake hint — a missed notification (LISTEN
// connection blip) self-heals on the next one, because a connection always
// re-reads from its own cursor, never trusts the payload to be a complete log.
type hub struct {
	pool *pgxpool.Pool

	mu    sync.Mutex
	byKey map[string]map[*conn]struct{}
}

func newHub(pool *pgxpool.Pool) *hub {
	return &hub{pool: pool, byKey: map[string]map[*conn]struct{}{}}
}

func (h *hub) add(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := h.byKey[c.sessionKey]
	if m == nil {
		m = map[*conn]struct{}{}
		h.byKey[c.sessionKey] = m
	}
	m[c] = struct{}{}
}

func (h *hub) remove(c *conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if m := h.byKey[c.sessionKey]; m != nil {
		delete(m, c)
		if len(m) == 0 {
			delete(h.byKey, c.sessionKey)
		}
	}
}

func (h *hub) wake(sessionKey string) {
	h.mu.Lock()
	conns := make([]*conn, 0, len(h.byKey[sessionKey]))
	for c := range h.byKey[sessionKey] {
		conns = append(conns, c)
	}
	h.mu.Unlock()
	for _, c := range conns {
		c.notify()
	}
}

// run holds the LISTEN connection for the life of the process, reconnecting
// with backoff. Returns only when ctx is cancelled.
func (h *hub) run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := h.listenOnce(ctx); err != nil && ctx.Err() == nil {
			log.Printf("mobile hub: LISTEN connection error: %v (retrying in %s)", err, backoff)
			select {
			case <-ctx.Done():
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
	}
}

func (h *hub) listenOnce(ctx context.Context) error {
	pc, err := h.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer pc.Release()

	if _, err := pc.Exec(ctx, "LISTEN mobile_stream"); err != nil {
		return err
	}
	log.Printf("mobile hub: listening on mobile_stream")

	// On (re)connect, wake every session this replica serves — notifications
	// sent while the connection was down were dropped by Postgres.
	h.mu.Lock()
	keys := make([]string, 0, len(h.byKey))
	for k := range h.byKey {
		keys = append(keys, k)
	}
	h.mu.Unlock()
	for _, k := range keys {
		h.wake(k)
	}

	for ctx.Err() == nil {
		n, err := pc.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		h.wake(n.Payload)
	}
	return ctx.Err()
}
