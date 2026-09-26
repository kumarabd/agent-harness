// Command automation is the cluster's control-plane Temporal worker
// (docs/components/gateway/web.md's Phase 2) — currently just
// TenantOnboardingWorkflow, but the intended home for any future
// infrastructure-automation workflow, not named after onboarding
// specifically. Runs against its own dedicated Temporal namespace,
// "system" (TEMPORAL_NAMESPACE default below), on its own task queue —
// deliberately separate from every tenant's own "agent-loop" queue
// (loop-worker/tenant-worker/Gateway), so a runaway onboarding can never
// starve real turn traffic and vice versa. Deployed once per cluster via
// agent-harness-shared, alongside loop-worker/web/router — it provisions
// tenants, so it can't itself be per-tenant.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/worker"

	"agent-harness/workflows/internal/automation/activities"
	automationworkflow "agent-harness/workflows/internal/automation/workflow"
	"agent-harness/workflows/internal/onboarding"
	"agent-harness/workflows/internal/router/registry"
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

// newMetricsHandler is an exact copy of loop-worker's/gateway's own
// function of the same name — duplicated rather than shared since these
// are independent binaries with no existing common package for this
// (docs/components/budget-guardrails.md's "Resolved: Metrics Export"),
// same reasoning cmd/gateway/main.go's own copy already states.
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

	metricsHandler := newMetricsHandler(envOrDefault("METRICS_BIND_ADDRESS", "0.0.0.0:9090"))
	temporalClient, err := client.Dial(client.Options{
		HostPort:       envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace:      envOrDefault("TEMPORAL_NAMESPACE", "system"),
		MetricsHandler: metricsHandler,
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer temporalClient.Close()

	a := &activities.Activities{
		Store:                  onboarding.New(pool),
		Registry:               registry.New(pool),
		TemporalAddress:        envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		NamespaceRetentionDays: envIntOrDefault("TENANT_NAMESPACE_RETENTION_DAYS", 30),
		ChartDir:               envOrDefault("TENANT_CHART_DIR", "/charts/agent-harness-tenant"),
		SharedChartDir:         envOrDefault("SHARED_CHART_DIR", "/charts/agent-harness-shared"),
		SharedRelease:          envOrDefault("SHARED_RELEASE_NAME", "harness"),
		SharedNamespace:        envOrDefault("SHARED_RELEASE_NAMESPACE", "agents"),
		ClerkIssuer:            os.Getenv("CLERK_ISSUER"),
		ClerkSecretKey:         os.Getenv("CLERK_SECRET_KEY"),
		GatewayPort:            envIntOrDefault("TENANT_GATEWAY_PORT", 8090),
		AgentBrainPort:         envIntOrDefault("TENANT_AGENT_BRAIN_PORT", 8080),
	}
	if a.ClerkIssuer == "" {
		log.Fatalf("CLERK_ISSUER is required — every generated tenant's gateway.web.clerkIssuer comes from this")
	}
	if a.ClerkSecretKey == "" {
		log.Fatalf("CLERK_SECRET_KEY is required — CreateClerkOrganization can't create an Organization without it")
	}

	taskQueue := envOrDefault("TEMPORAL_TASK_QUEUE", "system")
	w := worker.New(temporalClient, taskQueue, worker.Options{})
	w.RegisterWorkflow(automationworkflow.TenantOnboardingWorkflow)
	w.RegisterActivity(a)

	log.Printf("automation worker starting: temporal=%s task_queue=%s", a.TemporalAddress, taskQueue)
	if err := w.Run(worker.InterruptCh()); err != nil {
		log.Fatalf("automation worker stopped with error: %v", err)
	}
}
