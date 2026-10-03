package core

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"

	"agent-harness/shared/clerkauth"
)

// /maps/ must sit behind the same Clerk check as every other prefix: no token, no proxying.
func TestMapsRouteRequiresAuthentication(t *testing.T) {
	h := New(clerkauth.Config{}, 8090, 8080).Handler()

	req := httptest.NewRequest("POST", "/maps/api/v1/ingest/batch", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, 401, rec.Code)
	assert.JSONEq(t, `{"error":"invalid session token"}`, rec.Body.String())
}

func TestMapsEngineBaseURL(t *testing.T) {
	s := New(clerkauth.Config{}, 8090, 8080)
	tenant := s.tenant("user_2abc")
	assert.Equal(t, "http://maps-engine."+tenant.Slug+".svc.cluster.local:8080", tenant.MapsEngineBaseURL())

	tenant = s.WithMapsEnginePort(8181).tenant("user_2abc")
	assert.Equal(t, "http://maps-engine."+tenant.Slug+".svc.cluster.local:8181", tenant.MapsEngineBaseURL())
}
