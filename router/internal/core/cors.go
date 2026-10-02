package core

// CORS handling for the router — new as of 2026-09-24 (docs/components/
// gateway/web.md, "Resolved: Shared agent-web + Identity-Routing Router"):
// the browser now calls this router directly instead of going through
// agent-web's own nginx as a same-origin reverse proxy, so this is
// genuinely cross-origin traffic for the first time anywhere in this
// codebase. Structurally mirrors agent-brain's own real
// internal/cors/middleware.go (Config{Enabled, AllowedOrigins} + a plain
// net/http middleware) rather than inventing a new shape, but named/env-var
// wired like this repo's own sibling convention — gateway/internal/
// web/web.go's GATEWAY_WEB_ALLOWED_ORIGINS (comma-separated exact
// origins, no framework) — since that's the pattern already living next to
// this one in the same monorepo.

import (
	"net/http"
	"os"
	"strings"
)

type corsConfig struct {
	allowAll bool
	allowed  map[string]struct{}
}

// corsConfigFromEnv reads ROUTER_ALLOWED_ORIGINS — comma-separated exact
// origins (scheme+host[:port], no path), or "*" to allow any origin (the
// actual request Origin is still echoed back per-response rather than a
// literal "*", so this remains correct even for a future credentialed
// request mode). Same split/trim/skip-empty parsing as
// GATEWAY_WEB_ALLOWED_ORIGINS.
func corsConfigFromEnv() corsConfig {
	cfg := corsConfig{allowed: map[string]struct{}{}}
	for _, origin := range strings.Split(os.Getenv("ROUTER_ALLOWED_ORIGINS"), ",") {
		origin = strings.TrimSpace(origin)
		if origin == "" {
			continue
		}
		if origin == "*" {
			cfg.allowAll = true
			continue
		}
		cfg.allowed[origin] = struct{}{}
	}
	return cfg
}

func (c corsConfig) allows(origin string) bool {
	if c.allowAll {
		return true
	}
	_, ok := c.allowed[origin]
	return ok
}

// corsMiddleware sets CORS headers for an allowed Origin and short-circuits
// preflight OPTIONS requests. A request with no Origin header (a same-origin
// request, or a non-browser client) or a disallowed Origin passes through
// untouched — this middleware only ever adds headers, it never rejects a
// request itself; actual auth/authorization stays entirely in
// Server.authenticate/handleProxy, same separation of concerns as
// agent-brain's own cors.Middleware wrapping its real auth middleware.
func corsMiddleware(cfg corsConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && cfg.allows(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
			// Accept/Authorization/Content-Type/X-Agent-ID cover both
			// gateway.ts's and api.ts's Bearer-JWT calls (both send the live
			// Clerk token now — see proxy.go's headerVerifiedUser). X-API-Key
			// remains for legacy/self-hosted mode, where there's no Clerk
			// session and api.ts falls back to a pasted long-lived credential
			// (Login.tsx's dev form) that agent-brain verifies itself.
			w.Header().Set("Access-Control-Allow-Headers", "Accept, Authorization, Content-Type, X-API-Key, X-Agent-ID")
			w.Header().Set("Access-Control-Max-Age", "86400")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
