// Command router is the shared, identity-routing front door for agent-web
// (docs/components/gateway/web.md) — deployed ONCE for the whole cluster in
// agent-harness-shared, alongside agent-web and loop-worker. It verifies
// the caller's Clerk session JWT, resolves which tenant they belong to from
// their active Clerk organization (workflows/internal/router/registry), and
// reverse-proxies the request to that tenant's own per-namespace Gateway or
// agent-brain (workflows/internal/router/core) — it never becomes a shared
// credential store itself: no tenant secret is held here, only a
// namespace/release-name pointer per organization.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	temporalclient "go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/clerkauth"
	"agent-harness/workflows/internal/onboarding"
	"agent-harness/workflows/internal/router/core"
	"agent-harness/workflows/internal/router/registry"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pgURL := "postgres://" +
		envOrDefault("POSTGRES_USER", "router") + ":" +
		envOrDefault("POSTGRES_PASSWORD", "") + "@" +
		envOrDefault("POSTGRES_HOST", "localhost") + ":" +
		envOrDefault("POSTGRES_PORT", "5432") + "/" +
		envOrDefault("POSTGRES_DB", "router")
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("unable to connect to Postgres: %v", err)
	}
	defer pool.Close()

	// docs/components/gateway/web.md, "Resolved: Auth" — same fail-loud
	// startup discipline as workflows/cmd/gateway/main.go: this router can
	// never verify anyone without it, so don't come up half-broken.
	clerkCfg := clerkauth.ConfigFromEnv()
	if clerkCfg.JWKSURL == "" {
		log.Fatalf("CLERK_JWKS_URL or CLERK_ISSUER is required")
	}

	reg := registry.New(pool)
	srv := core.New(clerkCfg, reg)

	// Self-serve tenant onboarding (docs/components/gateway/web.md's Phase
	// 2, onboarding.go) — a second Temporal client, dialed against the
	// "system" namespace the automation worker itself runs on
	// (workflows/cmd/automation/main.go's own default), distinct from any
	// tenant's own namespace this router otherwise never touches directly.
	onboardingTemporal, err := temporalclient.Dial(temporalclient.Options{
		HostPort:  envOrDefault("TEMPORAL_ADDRESS", temporalclient.DefaultHostPort),
		Namespace: envOrDefault("TEMPORAL_NAMESPACE", "system"),
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client for onboarding: %v", err)
	}
	defer onboardingTemporal.Close()
	srv.WithOnboarding(onboarding.New(pool), onboardingTemporal, envOrDefault("AUTOMATION_TASK_QUEUE", "system"))

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
