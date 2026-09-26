// Package core is the shared router's request path: verify the caller's
// Clerk identity, resolve which tenant they belong to, and reverse-proxy
// the request to that tenant's own per-namespace Gateway or agent-brain —
// docs/components/gateway/web.md's "the web becomes shared, fronted by an
// identity-routing router" resolution. This package never re-implements or
// weakens the downstream auth: it forwards the original Authorization
// header unchanged, and each tenant's own Gateway/agent-brain keeps
// verifying it exactly as it does today (workflows/internal/gateway/web/
// auth.go) — the router only ever adds one more check in front (does this
// caller belong to ANY tenant at all), it never removes one.
package core

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/onboarding"
	"agent-harness/workflows/internal/router/registry"
)

var errMissingToken = errors.New("missing bearer token")

type Server struct {
	clerkCfg clerkauth.Config
	registry *registry.Registry

	// Self-serve tenant onboarding (onboarding.go, docs/components/gateway/
	// web.md's Phase 2) — both nil-able: a router deployed without
	// automation.enabled (workflows/cmd/router/main.go) simply doesn't
	// register /onboard at all, same optionality gateway.enabled already has
	// in agent-harness-tenant.
	onboarding          *onboarding.Store
	temporal            temporalclient.Client
	automationTaskQueue string
}

func New(clerkCfg clerkauth.Config, reg *registry.Registry) *Server {
	return &Server{clerkCfg: clerkCfg, registry: reg}
}

// WithOnboarding enables /onboard (onboarding.go) — a separate step from
// New(), not an extra constructor argument, so every existing New(...)
// call site (including this package's own tests) keeps working unchanged
// when onboarding support isn't wired up.
func (s *Server) WithOnboarding(store *onboarding.Store, temporal temporalclient.Client, automationTaskQueue string) *Server {
	s.onboarding = store
	s.temporal = temporal
	s.automationTaskQueue = automationTaskQueue
	return s
}

// Handler builds the router's full HTTP handler: /gateway/ and /brain/ —
// agent-web's own AGENT_BRAIN_API_URL/GATEWAY_API_URL point at this router
// with those two path prefixes (docker/runtime-config.js.template in the
// agent-web repo, not this one) instead of directly at one tenant's own
// Services; everything after the prefix is forwarded unchanged, so
// agent-web's own request paths (POST /send, GET /poll, ... —
// docs/components/gateway/web.md) don't need to change at all. Wrapped in
// corsMiddleware — 2026-09-24, since the browser now calls this router
// directly (agent-web's own nginx no longer reverse-proxies in front of
// it), making this genuinely cross-origin traffic.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Connection control is intentionally served by the shared router, not
	// proxied to a tenant hub: it owns the cross-tenant control-plane database
	// and only exposes the caller's own organization after Clerk verification.
	mux.HandleFunc("GET /gateway/connections", s.listConnections)
	mux.HandleFunc("POST /gateway/connections", s.requestConnection)
	mux.HandleFunc("POST /gateway/connections/{integration}/authorize", s.authorizeConnection)
	// OAuth providers do not carry the browser's Clerk bearer token. This
	// route only accepts callbacks; the hub validates its stored PKCE/state.
	mux.HandleFunc("GET /hub/{org}/oauth/{backend}/callback", s.connectionCallback)
	mux.HandleFunc("/gateway/", s.handleProxy("/gateway", func(t registry.Tenant) string { return t.GatewayBaseURL() }))
	mux.HandleFunc("/brain/", s.handleProxy("/brain", func(t registry.Tenant) string { return t.AgentBrainBaseURL() }))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if s.onboarding != nil {
		s.registerOnboarding(mux)
	}
	return corsMiddleware(corsConfigFromEnv(), mux)
}

func (s *Server) tenantForRequest(w http.ResponseWriter, r *http.Request) (registry.Tenant, bool) {
	orgID, err := s.authenticate(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return registry.Tenant{}, false
	}
	if orgID == "" {
		writeJSONError(w, http.StatusNotFound, "no_tenant")
		return registry.Tenant{}, false
	}
	tenant, err := s.registry.Lookup(r.Context(), orgID)
	if err != nil {
		if err == registry.ErrNotFound {
			writeJSONError(w, http.StatusNotFound, "no_tenant")
		} else {
			writeJSONError(w, http.StatusBadGateway, "registry lookup failed")
		}
		return registry.Tenant{}, false
	}
	return tenant, true
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenantForRequest(w, r)
	if !ok {
		return
	}
	items, err := s.registry.ListConnections(r.Context(), tenant.OrgID)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "connections unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "enabled": os.Getenv("CONNECTION_AUTOMATION_ENABLED") == "true"})
}

func (s *Server) requestConnection(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.tenantForRequest(w, r)
	if !ok {
		return
	}
	if os.Getenv("CONNECTION_AUTOMATION_ENABLED") != "true" {
		writeJSONError(w, http.StatusServiceUnavailable, "connection automation is not enabled")
		return
	}
	var input struct {
		IntegrationID string `json:"integration_id"`
		DesiredState  string `json:"desired_state"`
		Token         string `json:"token"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(&input); err != nil || input.IntegrationID == "" {
		writeJSONError(w, http.StatusBadRequest, "integration_id is required")
		return
	}
	if input.DesiredState == "" {
		input.DesiredState = "connected"
	}
	if input.DesiredState != "connected" && input.DesiredState != "disconnected" {
		writeJSONError(w, 400, "invalid desired_state")
		return
	}
	connection, err := s.registry.RequestConnection(r.Context(), tenant.OrgID, input.IntegrationID, input.DesiredState, strings.TrimSpace(input.Token))
	if err != nil {
		if errors.Is(err, registry.ErrConnectionBusy) {
			writeJSONError(w, 409, err.Error())
			return
		}
		if errors.Is(err, registry.ErrCredential) {
			writeJSONError(w, 400, err.Error())
			return
		}
		writeJSONError(w, http.StatusBadRequest, "unable to request connection")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(connection)
}

func hubURL(t registry.Tenant) string {
	return "http://" + t.ReleaseName + "-tools." + t.Namespace + ".svc.cluster.local:8000"
}
func (s *Server) authorizeConnection(w http.ResponseWriter, r *http.Request) {
	t, ok := s.tenantForRequest(w, r)
	if !ok {
		return
	}
	backend, err := s.registry.OAuthBackend(r.Context(), t.OrgID, r.PathValue("integration"))
	if err != nil {
		writeJSONError(w, 404, "connection not found")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", hubURL(t)+"/oauth/"+url.PathEscape(backend)+"/start", nil)
	if err != nil {
		writeJSONError(w, 502, "authorization unavailable")
		return
	}
	c := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		writeJSONError(w, 502, "authorization unavailable")
		return
	}
	defer res.Body.Close()
	location, e := url.Parse(res.Header.Get("Location"))
	if res.StatusCode < 300 || res.StatusCode >= 400 || e != nil || location.Scheme != "https" || location.Host == "" {
		writeJSONError(w, 502, "provider authorization unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]string{"authorization_url": location.String()})
}
func (s *Server) connectionCallback(w http.ResponseWriter, r *http.Request) {
	org, backend := r.PathValue("org"), r.PathValue("backend")
	t, err := s.registry.Lookup(r.Context(), org)
	if err != nil {
		writeJSONError(w, 404, "connection not found")
		return
	}
	name, err := s.registry.OAuthBackend(r.Context(), org, backend)
	if err != nil || name != backend {
		writeJSONError(w, 404, "connection not found")
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), "GET", hubURL(t)+"/oauth/"+url.PathEscape(name)+"/callback?"+r.URL.RawQuery, nil)
	if err != nil {
		writeJSONError(w, 400, "invalid callback")
		return
	}
	c := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		writeJSONError(w, 502, "authorization unavailable")
		return
	}
	defer res.Body.Close()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if res.StatusCode != 200 {
		w.WriteHeader(400)
		_, _ = io.WriteString(w, "Authorization failed or expired. Return to External systems and select Authorize again.")
		return
	}
	_, _ = io.WriteString(w, "Connected. You can close this tab and return to External systems; tools will appear after indexing.")
}

func (s *Server) handleProxy(prefix string, target func(registry.Tenant) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		orgID, err := s.authenticate(r)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid session token")
			return
		}
		if orgID == "" {
			// A real, valid Clerk user with no active organization — the
			// expected state for a freshly signed-up user who hasn't been
			// through onboarding yet (docs/components/gateway/web.md's
			// Phase 3). Not a 401: the token is fine, there's just nothing
			// to route to yet. agent-web's own onboarding CTA keys off this
			// exact status/code pair.
			writeJSONError(w, http.StatusNotFound, "no_tenant")
			return
		}

		tenant, err := s.registry.Lookup(r.Context(), orgID)
		if err != nil {
			if err == registry.ErrNotFound {
				writeJSONError(w, http.StatusNotFound, "no_tenant")
				return
			}
			log.Printf("registry lookup failed for org %s: %v", orgID, err)
			writeJSONError(w, http.StatusBadGateway, "registry lookup failed")
			return
		}

		targetURL, err := url.Parse(target(tenant))
		if err != nil {
			log.Printf("invalid tenant target URL for org %s: %v", orgID, err)
			writeJSONError(w, http.StatusBadGateway, "invalid tenant target")
			return
		}

		proxy := httputil.NewSingleHostReverseProxy(targetURL)
		originalDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			req.URL.Path = strings.TrimPrefix(req.URL.Path, prefix)
			if !strings.HasPrefix(req.URL.Path, "/") {
				req.URL.Path = "/" + req.URL.Path
			}
			originalDirector(req)
			// Authorization header is carried over as-is by the reverse
			// proxy's default director (it only rewrites URL/Host) — the
			// downstream Gateway/agent-brain re-verifies the same JWT
			// itself, deliberately: this router adds a check, it doesn't
			// become a trusted-identity boundary on its own.
		}
		proxy.ServeHTTP(w, r)
	}
}

// authenticate verifies the bearer token and extracts the caller's active
// Clerk organization id. Returns ("", nil) — not an error — for a valid
// token with no active org, and a non-nil error only for a missing/invalid
// token.
func (s *Server) authenticate(r *http.Request) (orgID string, err error) {
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return "", errMissingToken
	}
	claims, err := clerkauth.VerifyJWTClaims(r.Context(), s.clerkCfg, token)
	if err != nil {
		return "", err
	}
	return orgIDFromClaims(claims), nil
}

// authenticateUser verifies the bearer token and returns the caller's own
// Clerk user id (the "sub" claim) — unlike authenticate above, it does NOT
// require an active organization. Used by onboarding.go: a user submitting
// or polling an onboarding request is, by definition, someone who doesn't
// have a tenant/organization yet (or is checking on one still being
// created), so gating on orgID the way the proxy routes do would be
// self-defeating here.
func (s *Server) authenticateUser(r *http.Request) (userID string, err error) {
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return "", errMissingToken
	}
	return clerkauth.VerifyJWT(r.Context(), s.clerkCfg, token)
}

// orgIDFromClaims reads the active-organization id out of a verified Clerk
// session JWT. Clerk represents this two ways depending on how the session
// token is customized in the dashboard: a top-level "org_id" claim if the
// project's JWT template adds one explicitly, or the default nested
// "o": {"id": "org_..."} shape Clerk includes automatically once
// Organizations is enabled. Checked in that order so an explicit custom
// claim always wins; falls back to "" (no active org) if neither is
// present, e.g. a user who hasn't selected/created an organization yet.
func orgIDFromClaims(claims map[string]any) string {
	if v, ok := claims["org_id"].(string); ok && v != "" {
		return v
	}
	if o, ok := claims["o"].(map[string]any); ok {
		if v, ok := o["id"].(string); ok {
			return v
		}
	}
	return ""
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
