// Package mobile exposes the native phone/tablet WebSocket routes. iOS and
// Android share a wire protocol and implementation, but intentionally resolve
// to separate platform/session namespaces.
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

// Handler binds one native platform namespace to one explicit route.
type Handler struct {
	realtime *realtime.Handler
	route    string
}

// New keeps the original mobile endpoint available for already-installed
// clients while iOS and Android releases move to their explicit routes.
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	return newHandler(ctx, ingestor, pool, temporal, clerk, "mobile", "/ws")
}

// NewIOS wires the Apple mobile namespace. iPadOS and CarPlay intentionally
// share the iOS history because they are surfaces of the same Apple client.
func NewIOS(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	return newHandler(ctx, ingestor, pool, temporal, clerk, "ios", "/ios/ws")
}

// NewAndroid wires the Android namespace. Android Auto intentionally shares
// the Android history because it is a surface of the same Android client.
func NewAndroid(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	return newHandler(ctx, ingestor, pool, temporal, clerk, "android", "/android/ws")
}

func newHandler(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config, platform, route string) *Handler {
	return &Handler{route: route, realtime: realtime.New(ctx, ingestor, pool, temporal, clerk, realtime.Config{
		TrackPresence: true,
		CheckOrigin:   func(*http.Request) bool { return true },
		ResolveScope: func(userID, sessionID, parentSessionID string) (realtime.Scope, error) {
			if (sessionID != "" && sessionID != "main") || parentSessionID != "" {
				return realtime.Scope{}, errors.New(platform + " has one session")
			}
			return realtime.Scope{Platform: platform, Discriminator: "channel:" + userID}, nil
		},
	})}
}

// Register attaches the handler's explicit route. Native clients authenticate
// in their first WebSocket frame, preserving the shared deployed contract.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET "+h.route, h.realtime.HandleWS)
}
