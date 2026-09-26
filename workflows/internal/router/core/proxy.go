// Package core is the shared router's request path: verify the caller's
// Clerk identity, resolve which tenant they belong to, and reverse-proxy
// the request to that tenant's own per-namespace Gateway, agent-brain, or
// mcp-hub instance — docs/components/gateway/web.md's "the web becomes
// shared, fronted by an identity-routing router" resolution. This package
// never re-implements or weakens the downstream auth: it forwards the
// original Authorization header unchanged, and each tenant's own
// Gateway/agent-brain keeps verifying it exactly as before — the router
// only ever adds one more thing in front (compute which tenant this is), it
// never removes a check.
//
// 2026-09-26: the connections service (a Go worker that shelled out to
// `helm upgrade` to inject mcp-hub manifest config) is gone entirely —
// mcp-hub now owns its own connections end to end (its own Postgres-backed
// management API), and /connections/ here is a plain reverse proxy straight
// to that tenant's own mcp-hub instance. mcp-hub does no Clerk verification
// of its own (it predates this platform's auth model and isn't reachable
// except through this router) — the router's own JWT check is the only
// auth boundary for these routes, unlike /gateway/ and /brain/ where the
// downstream also re-verifies.
//
// 2026-09-25: tenant identity moved from a Postgres-backed tenant_registry
// lookup to pure convention (tenant.go's TenantForSub) — a tenant's
// Temporal namespace, Kubernetes namespace, and Helm release name are all
// the same string, derived deterministically from the caller's own Clerk
// user id. There is no more "register a tenant" step and no database
// anywhere in this router. The real, disclosed tradeoff: this router can no
// longer tell "not yet provisioned" from "provisioned but briefly
// unreachable" the cheap way (a lookup miss) — it now infers "no_tenant"
// from a DNS-not-found on the computed Service name (handleProxy's
// ErrorHandler), since a tenant's Kubernetes namespace/Services genuinely
// don't exist in DNS until `helm install` has actually run.
package core

import (
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
)

var errMissingToken = errors.New("missing bearer token")

// tenantSlugPattern mirrors workflows/internal/automation/activities/
// validate.go's own — used here only to validate an UNAUTHENTICATED path
// parameter (connectionCallback's tenant slug, embedded in an OAuth
// provider's callback URL) before using it to build a proxy target,
// defense against path-manipulation/SSRF via a crafted callback URL.
var tenantSlugPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{1,38}[a-z0-9])?$`)

type Server struct {
	clerkCfg clerkauth.Config

	gatewayPort    int
	agentBrainPort int

	// Self-serve tenant onboarding (onboarding.go, docs/components/gateway/
	// web.md's Phase 2) — nil-able: a router deployed without automation
	// wired up simply doesn't register /onboard at all, same optionality
	// gateway.enabled already has in agent-harness-tenant.
	temporal            temporalclient.Client
	automationTaskQueue string
}

func New(clerkCfg clerkauth.Config, gatewayPort, agentBrainPort int) *Server {
	return &Server{clerkCfg: clerkCfg, gatewayPort: gatewayPort, agentBrainPort: agentBrainPort}
}

// WithOnboarding enables /onboard (onboarding.go) — a separate step from
// New(), not an extra constructor argument, so every existing New(...)
// call site (including this package's own tests) keeps working unchanged
// when onboarding support isn't wired up.
func (s *Server) WithOnboarding(temporal temporalclient.Client, automationTaskQueue string) *Server {
	s.temporal = temporal
	s.automationTaskQueue = automationTaskQueue
	return s
}

func (s *Server) tenant(sub string) Tenant {
	return TenantForSub(sub, s.gatewayPort, s.agentBrainPort)
}

// Handler builds the router's full HTTP handler: /gateway/, /brain/, and
// /connections/ all forward to that tenant's own per-namespace services;
// everything after the prefix is forwarded unchanged, so agent-web's own
// request paths don't need to change at all. Wrapped in corsMiddleware,
// since the browser calls this router directly (cross-origin).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/gateway/", s.handleProxy("/gateway", func(t Tenant) string { return t.GatewayBaseURL() }))
	mux.HandleFunc("/brain/", s.handleProxy("/brain", func(t Tenant) string { return t.AgentBrainBaseURL() }))
	// Connection management — a pure reverse proxy straight to that tenant's
	// own mcp-hub instance's management API (GET/POST /api/connections,
	// DELETE /api/connections/{name}, GET /api/catalog), same shape as
	// /gateway/ and /brain/ above. The one exception is the authorize route
	// just below: mcp-hub's own /oauth/{backend}/start responds with a raw
	// 302, but the browser's popup-based consent flow needs the target URL
	// as a JSON value to open the popup at (see handleAuthorize) — that
	// translation has to happen server-side, so it's the router's own
	// handler, registered before the generic prefix (Go's ServeMux picks
	// the more specific pattern regardless of registration order).
	mux.HandleFunc("POST /connections/{backend}/authorize", s.handleAuthorize)
	mux.HandleFunc("/connections/", s.handleProxy("/connections", func(t Tenant) string { return t.McpHubBaseURL() }))
	// OAuth providers redirect back here with no Clerk bearer token at all —
	// the tenant slug is embedded directly in the callback URL (handleAuthorize
	// built it from the authenticated request that started the flow), not
	// derived from a session. handleOAuthCallback validates it against
	// tenantSlugPattern before using it.
	mux.HandleFunc("GET /hub/{tenant}/oauth/{backend}/callback", s.handleOAuthCallback)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	if s.temporal != nil {
		s.registerOnboarding(mux)
	}
	return corsMiddleware(corsConfigFromEnv(), mux)
}

func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	slug := r.PathValue("tenant")
	if !tenantSlugPattern.MatchString(slug) {
		writeJSONError(w, http.StatusNotFound, "connection not found")
		return
	}
	tenant := Tenant{Slug: slug, GatewayPort: s.gatewayPort, AgentBrainPort: s.agentBrainPort}
	targetURL, err := url.Parse(tenant.McpHubBaseURL())
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid tenant target")
		return
	}
	proxy := httputil.NewSingleHostReverseProxy(targetURL)
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		req.URL.Path = "/oauth/" + r.PathValue("backend") + "/callback"
		originalDirector(req)
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		writeJSONError(w, http.StatusNotFound, "connection not found")
	}
	proxy.ServeHTTP(w, r)
}

// handleAuthorize starts an OAuth connection flow on the caller's behalf and
// hands back the authorization URL as JSON rather than a redirect — the
// frontend opens a popup window first (pop-up-blocker workaround) and then
// navigates it to this URL, which only works if the URL is a value it can
// read, not a response it's redirected through. mcp-hub's own
// /oauth/{backend}/start responds with a raw 302; this fetches it with
// redirect-following disabled and reads the Location header server-side —
// the browser can't do this itself across origins (a `fetch` with
// `redirect: "manual"` yields an opaque response with no readable headers).
func (s *Server) handleAuthorize(w http.ResponseWriter, r *http.Request) {
	sub, err := s.authenticateUser(r)
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, "invalid session token")
		return
	}
	backend := r.PathValue("backend")
	tenant := s.tenant(sub)

	startURL := strings.TrimRight(tenant.McpHubBaseURL(), "/") + "/oauth/" + url.PathEscape(backend) + "/start"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, startURL, nil)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "invalid tenant target")
		return
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	res, err := client.Do(req)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
			writeJSONError(w, http.StatusNotFound, "no_tenant")
			return
		}
		log.Printf("authorize: mcp-hub request failed for tenant %s backend %s: %v", tenant.Slug, backend, err)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}
	defer res.Body.Close()

	if res.StatusCode < 300 || res.StatusCode >= 400 {
		writeJSONError(w, http.StatusBadGateway, "backend did not return an authorization redirect")
		return
	}
	location := res.Header.Get("Location")
	authURL, err := url.Parse(location)
	if err != nil || authURL.Scheme != "https" {
		log.Printf("authorize: mcp-hub returned an unusable redirect for tenant %s backend %s: %q", tenant.Slug, backend, location)
		writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"authorization_url": authURL.String()})
}

func (s *Server) handleProxy(prefix string, target func(Tenant) string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sub, err := s.authenticateUser(r)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid session token")
			return
		}

		tenant := s.tenant(sub)
		targetURL, err := url.Parse(target(tenant))
		if err != nil {
			log.Printf("invalid tenant target URL for sub %s: %v", sub, err)
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
			// downstream service re-verifies the same JWT itself,
			// deliberately: this router adds a check, it doesn't become a
			// trusted-identity boundary on its own.
		}
		proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
			var dnsErr *net.DNSError
			if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
				// The tenant's namespace/Service genuinely doesn't exist in
				// cluster DNS yet — this IS "not onboarded", the same
				// signal agent-web's isNoTenantError already expects.
				writeJSONError(w, http.StatusNotFound, "no_tenant")
				return
			}
			log.Printf("proxy error for tenant %s (%s): %v", tenant.Slug, prefix, err)
			writeJSONError(w, http.StatusBadGateway, "upstream unavailable")
		}
		proxy.ServeHTTP(w, r)
	}
}

// authenticateUser verifies the bearer token and returns the caller's own
// Clerk user id ("sub") — the one thing every proxy/onboarding route needs,
// now that tenant identity is derived from it directly instead of an active
// organization.
func (s *Server) authenticateUser(r *http.Request) (userID string, err error) {
	authHeader := r.Header.Get("Authorization")
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" || token == authHeader {
		return "", errMissingToken
	}
	return clerkauth.VerifyJWT(r.Context(), s.clerkCfg, token)
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
