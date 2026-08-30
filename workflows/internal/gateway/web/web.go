// Package web is the Web gateway platform: the authenticated HTTP surface
// (POST /send, GET /poll, POST /respond, GET /sessions) a browser client
// uses. docs/components/gateway/web.md. It normalizes each request into a
// core.MessageEvent and hands it to the shared core.Ingestor; delivery
// "collapses" for a polling client (GET /poll reads Postgres directly), so
// there is no embedded Temporal worker here the way Discord has.
package web

import (
	"encoding/json"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/core"
)

// Handler serves the Web gateway's HTTP routes.
type Handler struct {
	ingestor *core.Ingestor
	pool     *pgxpool.Pool
	temporal client.Client
	clerk    ClerkConfig
}

// New wires a Web Handler to its dependencies.
func New(ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, clerk ClerkConfig) *Handler {
	return &Handler{ingestor: ingestor, pool: pool, temporal: temporal, clerk: clerk}
}

// Register attaches the Web gateway's routes to mux, each behind Clerk auth.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.Handle("POST /send", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleSend)))
	mux.Handle("GET /poll", requireClerkAuth(h.clerk, http.HandlerFunc(h.handlePoll)))
	mux.Handle("POST /respond", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleRespond)))
	mux.Handle("GET /sessions", requireClerkAuth(h.clerk, http.HandlerFunc(h.handleListSessions)))
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
