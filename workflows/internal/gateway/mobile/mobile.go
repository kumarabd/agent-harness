// Package mobile is the Mobile gateway platform: an authenticated WebSocket
// (GET /ws) an iOS/iPad app connects to. docs/components/gateway/mobile.md.
//
// Inbound frames normalize into a core.MessageEvent and go through the shared
// core.Ingestor, exactly like web. Outbound is a realtime stream: the workflow
// writes to messages / turn_deliveries / turn_status_pings (unchanged), a
// NOTIFY trigger wakes this replica's hub, and each connection re-reads its own
// tail and pushes frames. One session per user, fanned out to every connected
// device — the client owns its resume cursor, the server keeps no per-device
// read state.
package mobile

import (
	"context"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/gateway/core"
)

// Handler serves the Mobile gateway's WebSocket route.
type Handler struct {
	ingestor *core.Ingestor
	pool     *pgxpool.Pool
	temporal client.Client
	clerk    clerkauth.Config
	hub      *hub
	upgrader websocket.Upgrader
}

// New wires a Mobile Handler and starts its per-replica LISTEN loop; the loop
// stops when ctx is cancelled.
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	h := &Handler{
		ingestor: ingestor,
		pool:     pool,
		temporal: temporal,
		clerk:    clerk,
		hub:      newHub(pool),
		upgrader: websocket.Upgrader{
			HandshakeTimeout: 10 * time.Second,
			// Same-origin isn't meaningful for a native app; auth is the
			// first frame, not the Origin header.
			CheckOrigin: func(*http.Request) bool { return true },
		},
	}
	go h.hub.run(ctx)
	return h
}

// Register attaches GET /ws. Auth is NOT a middleware here — the token comes
// in the first frame (some ingress strips non-standard headers on upgrade).
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ws", h.handleWS)
}

func clerkVerify(ctx context.Context, h *Handler, token string) (string, error) {
	return clerkauth.VerifyJWT(ctx, h.clerk, token)
}

func (h *Handler) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already wrote the error response
	}
	c := &conn{
		h:            h,
		ws:           ws,
		wakeCh:       make(chan struct{}, 1),
		sentThrough:  -1,
		sentMsgSeq:   -1,
		curTurnSeq:   -1,
	}
	c.serve(r.Context())
}
