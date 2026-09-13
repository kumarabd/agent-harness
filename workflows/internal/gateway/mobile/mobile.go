// Package mobile exposes the stable native-client WebSocket route. Its
// transport implementation lives in gateway/realtime so Web and macOS can
// reuse it without sharing mobile's conversation scope.
package mobile

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/realtime"
)

// Handler keeps GET /ws and the existing native frame contract intact.
type Handler struct {
	realtime *realtime.Handler
}

// New wires the native adapter to the reusable realtime transport.
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	return &Handler{realtime: realtime.New(ctx, ingestor, pool, temporal, clerk, realtime.Config{
		TrackPresence: true,
		CheckOrigin:   func(*http.Request) bool { return true },
		ResolveScope: func(userID, sessionID, parentSessionID string) (realtime.Scope, error) {
			if (sessionID != "" && sessionID != "main") || parentSessionID != "" {
				return realtime.Scope{}, errors.New("mobile has one session")
			}
			return realtime.Scope{Platform: "mobile", Discriminator: "channel:" + userID}, nil
		},
	})}
}

// Register attaches GET /ws. Native clients authenticate in their first
// WebSocket frame, preserving the existing deployed contract.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ws", h.realtime.HandleWS)
}
