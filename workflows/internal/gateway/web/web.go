// Package web is the Web gateway platform: the authenticated HTTP surface
// (POST /send, GET /poll, POST /respond, POST /cancel, GET /sessions) a
// browser client uses. docs/components/gateway/web.md. It normalizes each
// request into a
// core.MessageEvent and hands it to the shared core.Ingestor; delivery
// "collapses" for a polling client (GET /poll reads Postgres directly), so
// there is no embedded Temporal worker here the way Discord has.
package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/realtime"
)

// Handler serves the Web gateway's HTTP routes.
type Handler struct {
	ingestor *core.Ingestor
	pool     *pgxpool.Pool
	temporal client.Client
	clerk    clerkauth.Config
	realtime *realtime.Handler
}

// New wires a Web Handler to its dependencies.
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk clerkauth.Config) *Handler {
	h := &Handler{ingestor: ingestor, pool: pool, temporal: temporal, clerk: clerk}
	h.realtime = realtime.New(ctx, ingestor, pool, temporal, clerk, realtime.Config{
		CheckOrigin: webSocketOriginAllowed,
		ResolveScope: func(userID, sessionID, parentSessionID string) (realtime.Scope, error) {
			discriminator := webDiscriminator(userID, sessionID)
			parentSessionKey := ""
			if discriminator != "channel:"+userID {
				parentSessionKey = core.SessionKeyFor("web", userID, webDiscriminator(userID, parentSessionID))
			}
			return realtime.Scope{
				Platform:         "web",
				Discriminator:    discriminator,
				ParentSessionKey: parentSessionKey,
			}, nil
		},
	})
	return h
}

// Register attaches the Web gateway's routes to mux, each behind Clerk auth.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /send", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleSend)))
	mux.Handle("GET /poll", requireClerkAuth(h.clerk, http.HandlerFunc(h.handlePoll)))
	mux.Handle("POST /respond", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleRespond)))
	mux.Handle("POST /cancel", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleCancel)))
	mux.Handle("GET /sessions", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleListSessions)))
	// Browser authentication happens in the first WebSocket frame so this
	// shares the native realtime protocol. Origin verification is handled by
	// the upgrader rather than HTTP middleware.
	mux.HandleFunc("GET /web/ws", h.realtime.HandleWS)
}

// webSocketOriginAllowed accepts the same origin by default. Deployments that
// host the UI separately can list additional exact origins in
// GATEWAY_WEB_ALLOWED_ORIGINS, separated by commas. A missing Origin is
// rejected: this route is for browsers, unlike the native mobile route.
func webSocketOriginAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return false
	}
	if u.Host == r.Host {
		return true
	}
	for _, allowed := range strings.Split(os.Getenv("GATEWAY_WEB_ALLOWED_ORIGINS"), ",") {
		if strings.TrimSpace(allowed) == origin {
			return true
		}
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// envOr is web's own copy of the trivial env helper.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
