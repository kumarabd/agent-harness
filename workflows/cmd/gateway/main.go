// Command gateway is the per-tenant inbound/outbound path for every client
// platform this tenant has configured (docs/components/gateway.md). One
// process per tenant, deployed alongside tenant-worker and that tenant's own
// Postgres in the agent-harness-tenant chart.
//
// This file is the composition root only: it builds the shared
// infrastructure (Postgres pool, Temporal client, metrics) and the shared
// core.Ingestor, then wires each platform adapter and starts it. Everything
// else lives in internal/gateway/{core,lease,speech,web,discord,discordvoice,
// discordui}:
//
//	core         MessageEvent + Ingest (session resolve, dedup, SignalWithStart);
//	             session-key identity; per-platform system prompts.
//	lease        Postgres connection lease (Discord text + voice).
//	speech       OpenAI-compatible STT/TTS + transcript text helpers.
//	web          POST /send, GET /poll, POST /respond, GET /sessions (Clerk-auth).
//	discord      Discord text: connection, ingest, DiscordDeliver*, commands.
//	discordvoice Discord voice: capture, VoiceDeliver*, the voice DSP stack.
//	discordui    Discord message components shared by discord + discordvoice.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/discord"
	"agent-harness/workflows/internal/gateway/lease"
	"agent-harness/workflows/internal/gateway/web"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newMetricsHandler builds a Prometheus-backed client.MetricsHandler and
// starts the HTTP listener serving /metrics — an exact copy of loop-worker's
// own function (cmd/loop-worker/main.go), duplicated rather than shared
// since the two are independent binaries with no existing common package for
// this (docs/components/budget-guardrails.md's "Resolved: Metrics Export").
// Every activity on this process's embedded per-connection workers gets this
// handler automatically via activity.GetMetricsHandler(ctx).
func newMetricsHandler(bindAddress string) client.MetricsHandler {
	reporter := tallyprom.NewReporter(tallyprom.Options{})
	scope, _ := tally.NewRootScope(tally.ScopeOptions{
		CachedReporter:  reporter,
		SanitizeOptions: &contribtally.PrometheusSanitizeOptions,
		Separator:       "_",
	}, time.Second)
	scope = contribtally.NewPrometheusNamingScope(scope)

	mux := http.NewServeMux()
	mux.Handle("/metrics", reporter.HTTPHandler())
	go func() {
		if err := http.ListenAndServe(bindAddress, mux); err != nil { //nolint:gosec // internal-only exposition endpoint
			log.Printf("metrics HTTP server stopped: %v", err)
		}
	}()
	log.Printf("metrics exposition listening on %q", bindAddress)

	return contribtally.NewMetricsHandler(scope)
}

// discordBotTokens returns the configured Discord bot tokens for this tenant
// — DISCORD_BOT_TOKENS (comma-separated), with DISCORD_BOT_TOKEN (singular)
// kept as a one-bot fallback. Empty slice when neither is set: Discord is
// one of possibly several platform kinds, and having none is a valid state.
func discordBotTokens() []string {
	if raw := os.Getenv("DISCORD_BOT_TOKENS"); raw != "" {
		var out []string
		for _, tok := range strings.Split(raw, ",") {
			if tok = strings.TrimSpace(tok); tok != "" {
				out = append(out, tok)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if tok := os.Getenv("DISCORD_BOT_TOKEN"); tok != "" {
		return []string{tok}
	}
	return nil
}

func main() {
	ctx, stopPlatforms := context.WithCancel(context.Background())
	defer stopPlatforms()

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

	metricsHandler := newMetricsHandler(envOrDefault("METRICS_BIND_ADDRESS", "0.0.0.0:9090"))
	temporalClient, err := client.Dial(client.Options{
		HostPort:       envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace:      envOrDefault("TEMPORAL_NAMESPACE", client.DefaultNamespace),
		MetricsHandler: metricsHandler,
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer temporalClient.Close()

	// docs/components/gateway/web.md, "Resolved: Auth" — read once at startup,
	// fail loud (not per-request 503s) if the Gateway can never verify anyone.
	clerkCfg := web.ClerkConfigFromEnv()
	if clerkCfg.JWKSURL == "" {
		log.Fatalf("CLERK_JWKS_URL or CLERK_ISSUER is required")
	}

	taskQueue := envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop")
	ingestor := core.NewIngestor(pool, temporalClient, taskQueue)
	leaseMgr := lease.NewManager(pool)

	mux := http.NewServeMux()
	web.New(ingestor, pool, temporalClient, clerkCfg).Register(mux)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// One goroutine per configured Discord bot (gateway.md's per-tenant,
	// one-goroutine-per-platform model; a tenant can run more than one bot).
	for _, botToken := range discordBotTokens() {
		bot := discord.New(ctx, botToken, ingestor, pool, temporalClient, leaseMgr)
		go bot.Run(ctx)
	}

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
	stopPlatforms() // Discord goroutines release their leases and disconnect.

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
