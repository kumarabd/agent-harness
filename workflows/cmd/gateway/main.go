// Command gateway is the real inbound/outbound path for the Web platform
// (docs/components/gateway.md, docs/components/gateway/web.md) — the first
// real Gateway kind actually being built, replacing `starter` as the way a
// real client submits messages. One process per tenant (not shared across
// tenants — components/multi-tenancy.md's credential-isolation principle),
// deployed alongside tenant-worker and that tenant's own Postgres in the
// agent-harness-tenant chart, not agent-harness-shared.
//
// Web (webhook-like/polling) and Discord (connection-based, leased) both run
// in this one process — docs/components/gateway.md's "Resolved: Per-Tenant
// Deployment", one goroutine per platform kind this tenant has actually
// configured, not one process per (tenant × platform):
//
//	POST /send     — verify Clerk JWT, resolve session_key, dedup, SignalWithStart, ack.
//	GET  /poll     — verify Clerk JWT, resolve session_key, read new turns +
//	                 any pending user_input_requests directly from Postgres.
//	POST /respond  — verify Clerk JWT, answer a pending user_input_requests
//	                 row via SignalWorkflow against its own workflow_id
//	                 (docs/components/user-input.md).
//	Discord goroutine(s) (discord.go) — one per token in DISCORD_BOT_TOKENS
//	(comma-separated — a tenant can run more than one Discord bot,
//	gateway.md's composable connection_id); DISCORD_BOT_TOKEN (singular) is
//	still read as a one-bot fallback. Each connects only while holding that
//	bot's own connection lease (gateway_connection_leases, leases.go), per
//	gateway.md's "Resolved: Connection Leasing".
//
//	METRICS_BIND_ADDRESS Host:port the Prometheus exposition endpoint listens
//	                    on. Default: 0.0.0.0:9090. Same mechanism as
//	                    loop-worker/tenant-worker (docs/components/
//	                    budget-guardrails.md's "Resolved: Metrics Export"),
//	                    added here 2026-08-26 specifically for real voice-
//	                    latency numbers (discord-voice.md's Notes Log) —
//	                    this process had no metrics exposition at all before
//	                    that.
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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
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
// this (docs/components/budget-guardrails.md's "Resolved: Metrics Export"
// scoped this to loop-worker/tenant-worker only; the Gateway had none at
// all until this — real voice-latency metrics below needed somewhere to
// report to). Every activity registered on this process's embedded
// per-connection workers (deliver_voice.go, deliver_voice_chunk.go,
// deliver_discord.go) gets this handler automatically via
// activity.GetMetricsHandler(ctx), the same way tenant-worker's Python
// activities already get theirs via activity.metric_meter() — no per-struct
// plumbing needed for the metrics that are emitted from inside a real
// activity execution.
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

// discordBotTokens returns the configured Discord bot tokens for this
// tenant — DISCORD_BOT_TOKENS (comma-separated, deploy/helm/agent-harness-tenant's
// gateway.discord.bots list), same convention loop-worker's own
// TEMPORAL_NAMESPACES already uses, with DISCORD_BOT_TOKEN (singular) kept
// as a fallback for the pre-multi-bot single-value shape. Returns an empty
// slice (not an error) when neither is set — Discord is one of possibly
// several platform kinds this tenant's Gateway runs; having none configured
// is a valid, common state (main.go's own loop over this is a no-op then).
func discordBotTokens() []string {
	if raw := os.Getenv("DISCORD_BOT_TOKENS"); raw != "" {
		var out []string
		for _, tok := range strings.Split(raw, ",") {
			tok = strings.TrimSpace(tok)
			if tok != "" {
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

type server struct {
	pool      *pgxpool.Pool
	temporal  client.Client
	taskQueue string
	// voice — docs/components/gateway/discord-voice.md's per-guild live
	// voice connection registry. In-memory only, same reasoning as
	// discord.go's own dg session: gateway_connection_leases is the durable
	// record of who holds each connection, this is just this replica's own
	// bookkeeping of which ones it's currently serving.
	voice *voiceState
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
		voice:     newVoiceState(),
	}

	mux := http.NewServeMux()
	mux.Handle("POST /send", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handleSend)))
	mux.Handle("GET /poll", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handlePoll)))
	mux.Handle("POST /respond", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handleRespond)))
	mux.Handle("GET /sessions", requireClerkAuth(clerkCfg, http.HandlerFunc(s.handleListSessions)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	// discord.go — only this tenant's configured platform kinds run, per
	// gateway.md's per-tenant, one-goroutine-per-platform model. One
	// startDiscordPlatform goroutine per configured bot token (gateway.md's
	// composable connection_id — a tenant can run more than one Discord
	// bot), same comma-separated-list convention loop-worker's own
	// TEMPORAL_NAMESPACES already uses. holderID identifies this specific
	// process to gateway_connection_leases; a fresh one per bot per process
	// start is fine — the lease is about which process currently holds a
	// given live connection, not about recognizing a process across
	// restarts.
	for _, botToken := range discordBotTokens() {
		go s.startDiscordPlatform(ctx, botToken, uuid.NewString())
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
	stopPlatforms() // signals the Discord goroutine (if running) to release its lease and disconnect

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(shutdownCtx)
}
