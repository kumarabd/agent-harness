package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/clerk/clerk-sdk-go/v2/jwt"
)

type contextKey string

const clerkUserIDKey contextKey = "clerk_user_id"

// requireClerkAuth verifies the bearer token against Clerk's own JWKS
// (docs/components/gateway/web.md, "Resolved: Auth — Reuse agent-web's
// Existing Clerk Integration") and resolves the real Clerk user_id from the
// token's Subject claim — session_key is built from this, never trusted
// from anything the client sends directly.
func requireClerkAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		claims, err := jwt.Verify(r.Context(), &jwt.VerifyParams{Token: token})
		if err != nil {
			http.Error(w, "invalid session token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), clerkUserIDKey, claims.Subject)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func userIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clerkUserIDKey).(string)
	return id
}

// sessionKeyFor mirrors gateway/web.md's "Resolved: One Continuous Session
// Per User" scheme — one session_key per user, forever, no thread/conversation
// concept. Only place this string gets built, so the format only needs to be
// right in one location.
func sessionKeyFor(clerkUserID string) string {
	return "agent:main:web:user:" + clerkUserID
}
