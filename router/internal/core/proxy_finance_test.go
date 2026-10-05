package core

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"agent-harness/shared/clerkauth"
)

// /finance/ must sit behind the same Clerk check as every other prefix: no token, no proxying.
func TestFinanceRouteRequiresAuthentication(t *testing.T) {
	h := New(clerkauth.Config{}, 8090, 8080).Handler()

	req := httptest.NewRequest("POST", "/finance/api/v1/operations/spends_search", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, 401, rec.Code) // 401, not 404: the route exists and is gated
	assert.JSONEq(t, `{"error":"invalid session token"}`, rec.Body.String())
}

func TestFinanceEngineBaseURL(t *testing.T) {
	s := New(clerkauth.Config{}, 8090, 8080)
	tenant := s.tenant("user_2abc")
	assert.Equal(t, "http://finance-engine."+tenant.Slug+".svc.cluster.local:8091", tenant.FinanceEngineBaseURL())

	tenant = s.WithFinanceEnginePort(9191).tenant("user_2abc")
	assert.Equal(t, "http://finance-engine."+tenant.Slug+".svc.cluster.local:9191", tenant.FinanceEngineBaseURL())
}

// A real RS256 token against a real JWKS endpoint, so these tests exercise the same verification path production uses.
type tokenIssuer struct {
	key  *rsa.PrivateKey
	kid  string
	jwks *httptest.Server
}

func newTokenIssuer(t *testing.T, kid string) *tokenIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kid": kid, "kty": "RSA",
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		}}})
	}))
	t.Cleanup(jwks.Close)
	return &tokenIssuer{key: key, kid: kid, jwks: jwks}
}

func (i *tokenIssuer) token(t *testing.T, key *rsa.PrivateKey, sub string) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"sub": sub, "exp": time.Now().Add(time.Hour).Unix()})
	tok.Header["kid"] = i.kid
	signed, err := tok.SignedString(key)
	require.NoError(t, err)
	return signed
}

// What the finance engine actually receives through the router: the /finance prefix stripped, the verified user
// stamped from the token, and every identity header a client tried to send overwritten or removed.
func TestFinanceProxyStripsPrefixAndStampsIdentity(t *testing.T) {
	issuer := newTokenIssuer(t, "finance-test-kid")

	var gotPath, gotUser, gotActor, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotUser, gotActor = r.URL.Path, r.Header.Get(headerVerifiedUser), r.Header.Get(headerVerifiedActor)
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[],"total":0,"next_offset":null}`))
	}))
	defer upstream.Close()

	s := New(clerkauth.Config{JWKSURL: issuer.jwks.URL}, 8090, 8080)
	mux := http.NewServeMux()
	mux.HandleFunc("/finance/", s.handleProxy("/finance", func(Tenant) string { return upstream.URL }))

	req := httptest.NewRequest("POST", "/finance/api/v1/operations/spends_search", strings.NewReader(`{"limit":5}`))
	req.Header.Set("Authorization", "Bearer "+issuer.token(t, issuer.key, "user_2abc"))
	req.Header.Set(headerVerifiedUser, "attacker")  // a client cannot choose its own identity
	req.Header.Set(headerVerifiedActor, "attacker") // nor forge who appears in the audit log
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, 200, rec.Code)
	assert.JSONEq(t, `{"items":[],"total":0,"next_offset":null}`, rec.Body.String())
	assert.Equal(t, "/api/v1/operations/spends_search", gotPath)
	assert.Equal(t, "user_2abc", gotUser)
	assert.Equal(t, "", gotActor)
	assert.Equal(t, `{"limit":5}`, gotBody)
}

func TestFinanceProxyRejectsTokenSignedByAnotherKey(t *testing.T) {
	issuer := newTokenIssuer(t, "finance-test-kid-2")
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer upstream.Close()

	s := New(clerkauth.Config{JWKSURL: issuer.jwks.URL}, 8090, 8080)
	mux := http.NewServeMux()
	mux.HandleFunc("/finance/", s.handleProxy("/finance", func(Tenant) string { return upstream.URL }))

	req := httptest.NewRequest("POST", "/finance/api/v1/operations/spends_search", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+issuer.token(t, other, "user_2abc"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	assert.Equal(t, 401, rec.Code)
	assert.False(t, called, "a forged token must never reach the tenant's finance data")
}
