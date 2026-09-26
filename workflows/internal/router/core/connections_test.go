package core

import (
	"agent-harness/workflows/internal/gateway/clerkauth"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestConnectionRoutesRequireIdentity(t *testing.T) {
	handler := New(clerkauth.Config{}, nil).Handler()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/gateway/connections"}, {"POST", "/gateway/connections"},
		{"POST", "/gateway/connections/notion/authorize"},
	} {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(tc.method, tc.path, nil))
			require.Equal(t, http.StatusUnauthorized, response.Code)
		})
	}
}
