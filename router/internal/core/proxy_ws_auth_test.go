package core

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"agent-harness/shared/clerkauth"
)

// A browser's native WebSocket API cannot set an Authorization header on the
// upgrade request, so /gateway/web/ws carries the token as a `token` query
// parameter instead (gateway.ts's realtimeGatewayUrl). This must only be
// accepted for an actual WebSocket upgrade — never as a general way to
// authenticate a normal API call via the URL instead of the header.
func TestAuthenticateUserWebSocketQueryFallback(t *testing.T) {
	s := New(clerkauth.Config{}, 8090, 8080)

	t.Run("plain request with no header is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/gateway/web/ws", nil)
		_, err := s.authenticateUser(req)
		assert.ErrorIs(t, err, errMissingToken)
	})

	t.Run("websocket upgrade with no token anywhere is rejected", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/gateway/web/ws", nil)
		req.Header.Set("Upgrade", "websocket")
		_, err := s.authenticateUser(req)
		assert.ErrorIs(t, err, errMissingToken)
	})

	t.Run("plain (non-upgrade) request cannot authenticate via query token", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/gateway/web/ws?token=whatever", nil)
		_, err := s.authenticateUser(req)
		assert.ErrorIs(t, err, errMissingToken)
	})

	t.Run("websocket upgrade with a query token is not rejected as missing", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/gateway/web/ws?token=whatever", nil)
		req.Header.Set("Upgrade", "websocket")
		_, err := s.authenticateUser(req)
		// clerkauth.Config{} means verification itself can't succeed here,
		// but it must get past the "no token supplied" gate — a different
		// error than errMissingToken proves the query fallback was read.
		assert.NotErrorIs(t, err, errMissingToken)
	})
}
