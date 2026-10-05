package core

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCORSOriginMatching(t *testing.T) {
	t.Setenv("ROUTER_ALLOWED_ORIGINS", "https://mission-control.nighthawklabs.org, https://*.nighthawklabs.org ,http://localhost:5173")
	cfg := corsConfigFromEnv()

	cases := []struct {
		origin string
		want   bool
	}{
		{"https://mission-control.nighthawklabs.org", true}, // exact entry
		{"https://maps.nighthawklabs.org", true},            // wildcard, one level
		{"https://a.b.nighthawklabs.org", true},             // wildcard, several levels
		{"https://MAPS.NighthawkLabs.org", true},            // hosts are case-insensitive
		{"http://localhost:5173", true},                     // exact entry with a port
		{"https://nighthawklabs.org", false},                // the bare apex is not a subdomain
		{"http://maps.nighthawklabs.org", false},            // wrong scheme: the wildcard says https
		{"https://evilnighthawklabs.org", false},            // suffix must start at a dot
		{"https://nighthawklabs.org.evil.com", false},       // right text, wrong domain
		{"https://maps.nighthawklabs.org:8443", false},      // the pattern has no port
		{"https://evil.com/.nighthawklabs.org", false},      // not an origin: has a path
		{"https://evil.com?.nighthawklabs.org", false},      // not an origin: has a query
		{"https://maps.nighthawklabs.org@evil.com", false},  // userinfo trick
		{"null", false},
		{"", false},
	}
	for _, c := range cases {
		if got := cfg.allows(c.origin); got != c.want {
			t.Errorf("allows(%q) = %v, want %v", c.origin, got, c.want)
		}
	}
}

func TestCORSStarStillAllowsEverythingAndNoConfigAllowsNothing(t *testing.T) {
	t.Setenv("ROUTER_ALLOWED_ORIGINS", "*")
	if !corsConfigFromEnv().allows("https://anything.example") {
		t.Error(`"*" should allow any origin`)
	}
	t.Setenv("ROUTER_ALLOWED_ORIGINS", "")
	if corsConfigFromEnv().allows("https://maps.nighthawklabs.org") {
		t.Error("no configuration should allow nothing")
	}
}

func TestCORSMiddlewareEchoesAnAllowedWildcardOrigin(t *testing.T) {
	t.Setenv("ROUTER_ALLOWED_ORIGINS", "https://*.nighthawklabs.org")
	h := corsMiddleware(corsConfigFromEnv(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))

	req := httptest.NewRequest(http.MethodOptions, "/maps/api/v1/journeys", nil)
	req.Header.Set("Origin", "https://maps.nighthawklabs.org")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent || rec.Header().Get("Access-Control-Allow-Origin") != "https://maps.nighthawklabs.org" {
		t.Fatalf("preflight: %d, allow-origin %q", rec.Code, rec.Header().Get("Access-Control-Allow-Origin"))
	}

	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("a disallowed origin got CORS headers: %v", rec.Header())
	}
}
