// Command gateway is the real inbound/outbound path for the Web platform
// (docs/components/gateway.md, docs/components/gateway/web.md) — the first
// real Gateway kind actually being built, replacing `starter` as the way a
// real client submits messages. One process per tenant (not shared across
// tenants — components/multi-tenancy.md's credential-isolation principle),
// deployed alongside tenant-worker and that tenant's own Postgres in the
// agent-harness-tenant chart, not agent-harness-shared.
//
// Two plain HTTP endpoints, no embedded Temporal worker — Web is
// webhook-like/polling, not connection-based, so delivery collapses to a
// direct Postgres read (gateway/web.md's "Resolved: Delivery" section):
//
//	POST /send     — verify Clerk JWT, resolve session_key, dedup, SignalWithStart, ack.
//	GET  /poll     — verify Clerk JWT, resolve session_key, read new turns +
//	                 any pending user_input_requests directly from Postgres.
//	POST /respond  — verify Clerk JWT, answer a pending user_input_requests
//	                 row via SignalWorkflow against its own workflow_id
//	                 (docs/components/user-input.md).
//
// Scoped to Web only for this first pass — the connection-lease mechanism
// gateway.md designs for a future connection-based platform is deliberately
// not built here, since nothing needs it yet (this project's standing
// discipline against building ahead of real need, same reasoning already
// applied to model-registry's multi-provider question and memory-slot's
// dropped MemoryBackend interface).
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
	"go.temporal.io/sdk/client"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type server struct {
	pool      *pgxpool.Pool
	temporal  client.Client
	taskQueue string
}

func main() {
	ctx := context.Background()

	pgURL := "postgres://" +
		envOrDefault("POSTGRES_USER", "agent_harness") + ":" +
		envOrDefault("POSTGRES_PASSWORD", "") + "@" +
		envOrDefault("POSTGRES_HOST", "localhost") + ":" +
		envOrDefault("POSTGRES_PORT", "5432") + "/" +
		envOrDefault("POSTGRES_DB", "agent_harness")
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("unable to connect to Postgres: %v", err)
	}
	defer pool.Close()

	temporalClient, err := client.Dial(client.Options{
		HostPort:  envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace: envOrDefault("TEMPORAL_NAMESPACE", client.DefaultNamespace),
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer temporalClient.Close()

	// docs/components/gateway/web.md, "Resolved: Auth" — clerk.go, "Resolved
	// via agent-brain's own pattern": read once at startup, not re-read per
	// request. Fails loudly (not per-request 503s) if neither env var is
	// set — a Gateway that can never verify anyone is not a degraded state
	// worth serving traffic in, same as the Postgres/Temporal dial calls
	// above.
	clerkCfg := clerkConfigFromEnv()
	if clerkCfg.JWKSURL == "" {
		log.Fatalf("CLERK_JWKS_URL or CLERK_ISSUER is required")
	}

	s := &server{
		pool:      pool,
		temporal:  temporalClient,
		taskQueue: envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop"),
	}

	mux := http.NewServeMux()
	mux.Handle("POST /send", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handleSend)))
	mux.Handle("GET /poll", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handlePoll)))
	mux.Handle("POST /respond", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handleRespond)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	addr := envOrDefault("GATEWAY_BIND_ADDRESS", "0.0.0.0:8090")
	httpServer := &http.Server{Addr: addr, Handler: mux}

	go func() {
		log.Printf("gateway (web) listening on %s", addr)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http server failed: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
