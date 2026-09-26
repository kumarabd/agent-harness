// Command router is the shared, identity-routing front door for agent-web
// (docs/components/gateway/web.md) — deployed ONCE for the whole cluster in
// agent-harness-shared, alongside agent-web and loop-worker. It verifies the
// caller's Clerk session JWT and resolves which tenant they belong to by
// pure convention (workflows/internal/router/core/tenant.go — the tenant
// slug is derived deterministically from the caller's own Clerk user id,
// not looked up anywhere), then reverse-proxies to that tenant's own
// per-namespace Gateway, agent-brain, or connections service — it never
// becomes a shared credential store itself, and (2026-09-25) holds no
// database of any kind.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	temporalclient "go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/router/core"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func main() {
	// docs/components/gateway/web.md, "Resolved: Auth" — same fail-loud
	// startup discipline as workflows/cmd/gateway/main.go: this router can
	// never verify anyone without it, so don't come up half-broken.
	clerkCfg := clerkauth.ConfigFromEnv()
	if clerkCfg.JWKSURL == "" {
		log.Fatalf("CLERK_JWKS_URL or CLERK_ISSUER is required")
	}

	srv := core.New(clerkCfg,
		envIntOrDefault("TENANT_GATEWAY_PORT", 8090),
		envIntOrDefault("TENANT_AGENT_BRAIN_PORT", 8080),
	)

	// Self-serve tenant onboarding (docs/components/gateway/web.md's Phase
	// 2, onboarding.go) — a Temporal client dialed against the "system"
	// namespace the automation worker itself runs on, distinct from any
	// tenant's own namespace this router otherwise never touches directly.
	onboardingTemporal, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  envOrDefault("TEMPORAL_ADDRESS", temporalclient.DefaultHostPort),
		Namespace: envOrDefault("TEMPORAL_NAMESPACE", "system"),
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client for onboarding: %v", err)
	}
	defer onboardingTemporal.Close()
	srv.WithOnboarding(onboardingTemporal, envOrDefault("AUTOMATION_TASK_QUEUE", "system"))

	addr := envOrDefault("ROUTER_BIND_ADDRESS", "0.0.0.0:8080")
	httpServer := &http.Server{Addr: addr, Handler: srv.Handler()}
	go func() {
		log.Printf("router listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
