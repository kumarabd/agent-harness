// Command loop-worker registers the Session Coordinator and Turn Workflow on
// the configured task queue and polls a Temporal server for work. Run
// alongside every tenant's tenant-worker (activities/activities/tenant_worker.py),
// which polls that TENANT'S OWN task queue for the ModelCall/ToolCall/
// InsertMessage/Persist/Deliver/CompressContext activities referenced by
// name from the workflows here — this process itself never touches Postgres
// or holds any tenant's credentials; it only orchestrates.
//
// 2026-09-26: simplified from one Client+Worker pair per tenant Temporal
// namespace (a static TEMPORAL_NAMESPACES list, requiring a `helm upgrade`
// of this whole shared release every time a tenant was onboarded) down to a
// single Client+Worker pair on one shared namespace — docs/components/
// multi-tenancy.md's "Resolved: Shared Temporal Namespace, Per-Tenant Task
// Queues". This works because the workflow code itself (turn.go,
// coordinator.go, ...) is genuinely tenant-agnostic orchestration: it never
// holds a tenant's credentials, and every activity dispatch is routed to
// that ACTIVITY's own tenant-specific queue explicitly (workflow.
// TenantActivityQueue, via each workflow input's TenantSlug field) rather
// than by which namespace this process happened to be polling. So this one
// process, on one fixed queue, can safely run any tenant's workflow
// instance — onboarding a new tenant never touches this chart again.
//
// Configured via env vars (not hardcoded) so this binary is deployable —
// see deploy/docker/loop-worker.Dockerfile and
// deploy/helm/agent-harness-shared (this binary is the tenant-agnostic
// shared pool; deploy/helm/agent-harness-tenant deploys everything else,
// per docs/components/multi-tenancy.md):
//
//	TEMPORAL_ADDRESS    Temporal frontend host:port. Default: localhost:7233.
//	TEMPORAL_NAMESPACE  The one shared Temporal namespace every tenant's own
//	                    workers and this pool all use. Default: default.
//	TEMPORAL_TASK_QUEUE The shared workflow-task queue every tenant's own
//	                    Gateway starts CoordinatorWorkflow/TurnWorkflow on.
//	                    Default: agent-loop. Fixed — never tenant-specific
//	                    (that's TenantActivityQueue's job, applied inside the
//	                    workflow code itself, not this process's own config).
//	METRICS_BIND_ADDRESS Host:port the Prometheus exposition endpoint listens
//	                    on. Default: 0.0.0.0:9090. See
//	                    docs/components/budget-guardrails.md, "Resolved:
//	                    Metrics Export" — plain scrape, no ServiceMonitor.
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/uber-go/tally/v4"
	tallyprom "github.com/uber-go/tally/v4/prometheus"
	"go.temporal.io/sdk/client"
	contribtally "go.temporal.io/sdk/contrib/tally"
	"go.temporal.io/sdk/worker"
	temporalworkflow "go.temporal.io/sdk/workflow"

	wf "agent-harness/workflows/internal/workflow"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newMetricsHandler builds a Prometheus-backed client.MetricsHandler and
// starts the HTTP listener serving /metrics (docs/components/
// budget-guardrails.md, "Resolved: Metrics Export"). Per-tenant attribution
// happens via tags applied where workflow code actually calls
// workflow.GetMetricsHandler(ctx) (workflows/internal/workflow/turn.go), not
// here — this handler itself is tenant-agnostic.
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

// run starts the single shared Client+Worker pair and blocks until it stops
// — cleanly, once ctx is cancelled (returns nil), or with an error (dial
// failure, or Run() itself failing).
func run(ctx context.Context, address, namespace, taskQueue string, metricsHandler client.MetricsHandler) error {
	c, err := client.Dial(client.Options{HostPort: address, Namespace: namespace, MetricsHandler: metricsHandler})
	if err != nil {
		return err
	}
	defer c.Close()

	// LocalActivityWorkerOnly: this process registers no activities — every
	// activity (ModelCall, ToolCall, InsertMessage, Persist, Deliver,
	// CompressContext) is implemented by each tenant's own tenant-worker
	// (activities/activities/tenant_worker.py), on that tenant's own queue.
	// Without this flag, this worker would also poll for regular activity
	// tasks on ITS OWN queue and occasionally win that race, failing the
	// task since it has no implementation registered.
	w := worker.New(c, taskQueue, worker.Options{LocalActivityWorkerOnly: true})
	w.RegisterWorkflow(wf.CoordinatorWorkflow)
	w.RegisterWorkflow(wf.TurnWorkflow)
	w.RegisterWorkflow(wf.WriteMemoryWorkflow)
	w.RegisterWorkflow(wf.CompressContextWorkflow)
	w.RegisterWorkflow(wf.UserInputRequestWorkflow)
	w.RegisterWorkflow(wf.IntentionWorkflow)
	// docs/05-architecture-domain-control-loops.md — a mode-based turn
	// (session-persistent, dispatched directly by CoordinatorWorkflow via
	// tools.switch_mode, never by the model calling a tool) is a real
	// turn-shaped entry point (types.TurnInput/TurnResult, runTurn), so it's
	// registered here directly, under the exact string switch_mode's own
	// `mode` argument names — which must also be a key in
	// activities/activities/llm.py's MODE_TURNS. 2026-09-27: this is now the
	// ONLY skill-dispatch shape — the earlier one-shot skill mechanism
	// (types.SkillWorkflowInput, turn.go's runSkill, its own
	// workflows/internal/workflow/skills/ package) is deleted outright, not
	// kept alongside this as a second option: a skill whose own activity is
	// naturally single-round (service_monitoring) is still just a mode whose
	// curated prompt calls switch_mode() back to chat as soon as that one
	// round concludes, not a structurally different dispatch shape. Each
	// mode's own `skills` table row (migration 040_skills.sql) is kept, now
	// serving only as the per-tenant enable/disable gate (llm.ENABLED_MODES),
	// not a dispatch target.
	w.RegisterWorkflowWithOptions(wf.JournalingModeTurn, temporalworkflow.RegisterOptions{Name: "journaling"})
	w.RegisterWorkflowWithOptions(wf.ServiceMonitoringModeTurn, temporalworkflow.RegisterOptions{Name: "service_monitoring"})

	log.Printf("loop worker starting: temporal=%q namespace=%q task_queue=%q", address, namespace, taskQueue)

	// worker.Run wants a <-chan interface{}; adapt ctx's cancellation into
	// that shape.
	stopCh := make(chan interface{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	return w.Run(stopCh)
}

// retryRun keeps the Client+Worker pair alive for the life of the process: a
// failure to start or run it (the namespace not existing yet, a transient
// network blip, ...) is logged and retried with exponential backoff rather
// than treated as fatal.
func retryRun(ctx context.Context, address, namespace, taskQueue string, metricsHandler client.MetricsHandler) {
	const (
		initialBackoff = time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	for ctx.Err() == nil {
		err := run(ctx, address, namespace, taskQueue, metricsHandler)
		if err == nil {
			return // ctx was cancelled — clean shutdown, nothing to retry
		}
		log.Printf("loop worker stopped with error, retrying in %s: %v", backoff, err)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func main() {
	address := envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort)
	namespace := envOrDefault("TEMPORAL_NAMESPACE", client.DefaultNamespace)
	taskQueue := envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop")
	metricsHandler := newMetricsHandler(envOrDefault("METRICS_BIND_ADDRESS", "0.0.0.0:9090"))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	retryRun(ctx, address, namespace, taskQueue, metricsHandler)
	log.Printf("loop worker stopped, exiting")
}
