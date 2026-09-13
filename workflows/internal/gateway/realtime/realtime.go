// Package realtime provides the shared first-party WebSocket transport.
//
// It owns connection lifecycle, resumable catch-up and live delivery. Platform
// adapters own the route, browser-origin policy, and session scope; this keeps
// transport reuse from accidentally merging Web and mobile conversations.
package realtime

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/gateway/core"
)

// Scope is the server-derived conversation identity for one connection.
// Platform is intentionally chosen by the adapter, never trusted from a
// client frame. Discriminator is always a core.SessionKeyFor discriminator.
type Scope struct {
	Platform         string
	Discriminator    string
	ParentSessionKey string
}

// ResolveScope maps an authenticated user and optional client-facing session
// ID to a conversation. An adapter returns an error for a session it does not
// allow (for example, any non-main session requested by mobile).
type ResolveScope func(userID, sessionID, parentSessionID string) (Scope, error)

// Config is supplied by a platform adapter. The generic transport does not
// know whether it is serving a browser, phone, or desktop app.
type Config struct {
	ResolveScope  ResolveScope
	CheckOrigin   func(*http.Request) bool
	TrackPresence bool
}

// Handler serves one realtime adapter route.
type Handler struct {
	ingestor      *core.Ingestor
	pool          *pgxpool.Pool
	temporal      client.Client
	clerk         clerkauth.Config
	hub           *hub
	resolveScope  ResolveScope
	trackPresence bool
	upgrader      websocket.Upgrader
}

// New constructs a reusable realtime handler and starts its replica-local
// notification loop. Database notifications are wake hints only; catch-up
// always re-reads durable data from the connection's cursor.
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config, cfg Config) *Handler {
	if cfg.ResolveScope == nil {
		panic("realtime.New: ResolveScope is required")
	}
	checkOrigin := cfg.CheckOrigin
	if checkOrigin == nil {
		checkOrigin = func(*http.Request) bool { return false }
	}
	h := &Handler{
		ingestor:      ingestor,
		pool:          pool,
		temporal:      temporal,
		clerk:         clerk,
		hub:           newHub(pool),
		resolveScope:  cfg.ResolveScope,
		trackPresence: cfg.TrackPresence,
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			CheckOrigin:      checkOrigin,
		},
	}
	go h.hub.run(ctx)
	return h
}

// HandleWS upgrades and serves one authenticated realtime connection. Auth is
// deliberately the first application frame so native and browser clients use
// the same contract.
func (h *Handler) HandleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{
		h:           h,
		ws:          ws,
		wakeCh:      make(chan struct{}, 1),
		resumeCh:    make(chan int),
		deadCh:      make(chan struct{}),
		sentThrough: -1,
		sentMsgSeq:  -1,
		curTurnSeq:  -1,
		sentTools:   make(map[string]string),
	}
	c.serve(r.Context())
}

func (h *Handler) verify(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", errors.New("missing token")
	}
	return clerkauth.VerifyJWT(ctx, h.clerk, token)
}
