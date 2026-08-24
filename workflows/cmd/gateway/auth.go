package main

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const clerkUserIDKey contextKey = "clerk_user_id"

// requireClerkAuth verifies the bearer token against the Clerk project's own
// public JWKS (clerk.go, docs/components/gateway/web.md "Resolved: Auth —
// Reuse agent-web's Existing Clerk Integration") and resolves the real Clerk
// user_id from the token's sub claim — session_key is built from this, never
// trusted from anything the client sends directly.
func requireClerkAuth(cfg clerkConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		sub, err := verifyClerkSessionJWT(r.Context(), cfg, token)
		if err != nil {
			http.Error(w, "invalid session token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), clerkUserIDKey, sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func userIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clerkUserIDKey).(string)
	return id
}

// sessionKeyFor derives a session_key from a MessageEvent's Platform +
// ChannelID (inbound.go) — the session-scoping identity, deliberately NOT a
// function of User too (a group channel's session is shared across every
// user posting in it; see MessageEvent's own doc comment). Only place this
// string gets built, so the format only needs to be right in one location.
//
// The actual per-platform FORMAT is deliberately not generalized yet — only
// "web" exists, mirroring gateway/web.md's "Resolved: One Continuous Session
// Per User" scheme unchanged from before this refactor (changing the format
// would orphan already-running real sessions, a real behavior change this
// refactor isn't making). A second platform will need its own case here,
// almost certainly a different shape (gateway.md's own illustrative example,
// "agent:main:discord:guild:123", has a middle segment this one doesn't).
func sessionKeyFor(platform, channelID string) string {
	switch platform {
	case "web":
		return "agent:main:web:user:" + channelID
	default:
		panic("sessionKeyFor: unsupported platform " + platform)
	}
}
