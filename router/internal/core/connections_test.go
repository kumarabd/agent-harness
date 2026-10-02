package core

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"agent-harness/shared/clerkauth"
)

// 2026-09-26: /connections/ proxies straight to each tenant's own mcp-hub
// instance now (no more separate connections service) — see proxy.go's
// Handler(). The /authorize route is the one router-side exception (see
// handleAuthorize's own comment); auth is still checked the same way.
func TestConnectionRoutesRequireIdentity(t *testing.T) {
	handler := New(clerkauth.Config{}, 8090, 8080).Handler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/connections/"}, {"POST", "/connections/"},
		{"POST", "/connections/notion/authorize"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			require.Equal(t, http.StatusUnauthorized, response.Code)
		})
	}
}
