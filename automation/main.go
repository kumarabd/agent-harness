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
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/worker"

	"agent-harness/automation/activities"
	automationworkflow "agent-harness/automation/workflow"
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
	metricsHandler := newMetricsHandler(envOrDefault("METRICS_BIND_ADDRESS", "0.0.0.0:9090"))
	// This worker's OWN Temporal namespace ("system") — where
	// TenantOnboardingWorkflow itself actually runs — is deliberately
	// separate from TENANT_TEMPORAL_NAMESPACE below ("agents", the shared
	// namespace every tenant's turn-processing uses). This worker never
	// dials the latter itself; it only writes that string into a new
	// tenant's own generated values (docs/components/multi-tenancy.md's
	// "Resolved: Shared Temporal Namespace, Per-Tenant Task Queues").
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
		TemporalAddress: envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		// 2026-09-26: named "agents" to match the Kubernetes namespace the
		// shared release (and, by convention, every tenant) already lives
		// in — a deliberate, purely cosmetic consistency choice, not a
		// functional requirement (Temporal namespaces and Kubernetes
		// namespaces are unrelated concepts that happen to share this one
		// string now).
		TenantTemporalNamespace: envOrDefault("TENANT_TEMPORAL_NAMESPACE", "agents"),
		ChartDir:                envOrDefault("TENANT_CHART_DIR", "/charts/agent-harness-tenant"),
		SharedNamespace:         envOrDefault("SHARED_RELEASE_NAMESPACE", "agents"),
		ClerkIssuer:             os.Getenv("CLERK_ISSUER"),
		WebOrigin:               os.Getenv("WEB_ORIGIN"),
		RouterPublicURL:         os.Getenv("ROUTER_PUBLIC_URL"),
		GatewayPort:             envIntOrDefault("TENANT_GATEWAY_PORT", 8090),
		AgentBrainPort:          envIntOrDefault("TENANT_AGENT_BRAIN_PORT", 8080),
	}
	if a.ClerkIssuer == "" {
		log.Fatalf("CLERK_ISSUER is required — every generated tenant's gateway.web.clerkIssuer comes from this")
	}
	if a.WebOrigin == "" {
		log.Fatalf("WEB_ORIGIN is required — every generated tenant's gateway.web.allowedOrigins comes from this (agent-web's own public origin, e.g. https://mission-control.nighthawklabs.org)")
	}
	if a.RouterPublicURL == "" {
		log.Fatalf("ROUTER_PUBLIC_URL is required — every generated tenant's mcp-hub.oauth.mcpHubBaseUrl comes from this (the shared router's own public URL, e.g. https://harness-router.nighthawklabs.org), without which every self-serve tenant's OAuth backends (e.g. Notion) complete consent against the wrong mcp-hub instance")
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
