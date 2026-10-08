package core

import (
	"agent-harness/shared/clerkauth"
	"bytes"
	"github.com/stretchr/testify/assert"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetroRoutesRequireAuthentication(t *testing.T) {
	h := New(clerkauth.Config{}, 8090, 8080).Handler()
	for _, path := range []string{"/retro/api/v1/operations/garments_list", "/retro/api/v1/media/test/content", "/retro/mcp"} {
		r := httptest.NewRequest("POST", path, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		assert.Equal(t, 401, w.Code)
	}
	s := New(clerkauth.Config{}, 8090, 8080)
	tenant := s.tenant("user_retro")
	assert.Equal(t, "http://retro-engine."+tenant.Slug+".svc.cluster.local:8092", tenant.RetroEngineBaseURL())
	assert.Equal(t, 9192, s.WithRetroEnginePort(9192).tenant("user_retro").RetroEnginePort)
}
func TestRetroProxyForwardsBytesAndVerifiedIdentity(t *testing.T) {
	issuer := newTokenIssuer(t, "retro-photo-kid")
	payload := []byte{0xff, 0xd8, 0x00, 0x42}
	var gotBody []byte
	var gotPath, gotOwner, gotActor, gotMethod string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotOwner = r.Header.Get(headerVerifiedUser)
		gotActor = r.Header.Get(headerVerifiedActor)
		gotMethod = r.Method
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer upstream.Close()
	s := New(clerkauth.Config{JWKSURL: issuer.jwks.URL}, 8090, 8080)
	mux := http.NewServeMux()
	mux.HandleFunc("/retro/", s.handleProxy("/retro", func(Tenant) string { return upstream.URL }))
	r := httptest.NewRequest("PUT", "/retro/api/v1/media/id/content", bytes.NewReader(payload))
	r.Header.Set("Authorization", "Bearer "+issuer.token(t, issuer.key, "user_retro"))
	r.Header.Set(headerVerifiedUser, "attacker")
	r.Header.Set(headerVerifiedActor, "attacker")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	assert.Equal(t, 200, w.Code)
	assert.Equal(t, "PUT", gotMethod)
	assert.Equal(t, "/api/v1/media/id/content", gotPath)
	assert.Equal(t, "user_retro", gotOwner)
	assert.Empty(t, gotActor)
	assert.Equal(t, payload, gotBody)
}
