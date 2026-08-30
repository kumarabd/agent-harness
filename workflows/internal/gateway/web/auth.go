package web

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
func requireClerkAuth(cfg ClerkConfig, next http.Handler) http.Handler {
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
