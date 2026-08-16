// Command loop-worker registers the Session Coordinator and Turn Workflow on
// the configured task queue and polls a Temporal server for work. Run
// alongside each tenant's tenant-worker (activities/activities/tenant_worker.py),
// which polls the same task queue for the ModelCall/ToolCall/InsertMessage/
// Persist/Deliver/CompressContext activities referenced by name from the
// workflows here.
//
// Configured via env vars (not hardcoded) so this binary is deployable —
// see deploy/docker/loop-worker.Dockerfile and
// deploy/helm/agent-harness-shared (this binary is the tenant-agnostic
// shared pool; deploy/helm/agent-harness-tenant deploys everything else,
// per docs/components/multi-tenancy.md):
//
//	TEMPORAL_ADDRESS    Temporal frontend host:port. Default: localhost:7233.
//	TEMPORAL_NAMESPACE  Single Temporal namespace. Default: default. Used only
//	                    if TEMPORAL_NAMESPACES is unset — backward-compat with
//	                    single-tenant local-dev usage.
//	TEMPORAL_NAMESPACES Comma-separated list of Temporal namespaces this
//	                    process serves — one Client+Worker pair per namespace,
//	                    run concurrently in this single process
//	                    (docs/components/multi-tenancy.md, "Resolved: Compute
//	                    Isolation" — the shared, tenant-agnostic loop-worker
//	                    pool). Static config, chosen deliberately over dynamic
//	                    registration, mirroring the gateway's static-shard-config
//	                    pattern (components/gateway.md). Takes precedence over
//	                    TEMPORAL_NAMESPACE if both are set.
//	TEMPORAL_TASK_QUEUE Task queue name, shared across every namespace this
//	                    process serves. Default: agent-loop. Must match each
//	                    tenant's own tenant-worker fleet's TEMPORAL_TASK_QUEUE.
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	wf "agent-harness/workflows/internal/workflow"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// namespaces returns the list of tenant namespaces this process should serve,
// per the TEMPORAL_NAMESPACES/TEMPORAL_NAMESPACE resolution described above.
// Always returns at least one entry.
func namespaces() []string {
	if raw := os.Getenv("TEMPORAL_NAMESPACES"); raw != "" {
		var out []string
		for _, ns := range strings.Split(raw, ",") {
			ns = strings.TrimSpace(ns)
			if ns != "" {
				out = append(out, ns)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	return []string{envOrDefault("TEMPORAL_NAMESPACE", client.DefaultNamespace)}
}

// runForNamespace starts one Client+Worker pair for a single tenant namespace
// and blocks until it stops — cleanly, once ctx is cancelled (returns nil),
// or with an error (dial failure, or Run() itself failing).
func runForNamespace(ctx context.Context, address, namespace, taskQueue string) error {
	c, err := client.Dial(client.Options{HostPort: address, Namespace: namespace})
	if err != nil {
		return err
	}
	defer c.Close()

	// LocalActivityWorkerOnly: this process registers no activities — every
	// activity (ModelCall, ToolCall, InsertMessage, Persist, Deliver,
	// CompressContext) is implemented by that tenant's tenant-worker
	// (activities/activities/tenant_worker.py). Without this flag, this
	// worker would also poll for regular activity tasks on the same queue
	// and occasionally win that race, failing the task since it has no
	// implementation registered.
	w := worker.New(c, taskQueue, worker.Options{LocalActivityWorkerOnly: true})
	w.RegisterWorkflow(wf.CoordinatorWorkflow)
	w.RegisterWorkflow(wf.TurnWorkflow)

	log.Printf("loop worker starting: temporal=%q namespace=%q task_queue=%q", address, namespace, taskQueue)

	// worker.Run wants a <-chan interface{}; adapt ctx's cancellation into
	// that shape. Using one shared ctx (via signal.NotifyContext, registered
	// once in main) rather than each goroutine calling worker.InterruptCh()
	// independently: ctx.Done() is a broadcast, safely observed by however
	// many concurrent selects are waiting on it, whereas sharing one raw
	// signal.Notify channel across goroutines would only wake ONE waiter per
	// signal delivery — wrong for N concurrently-running namespace pairs.
	stopCh := make(chan interface{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	return w.Run(stopCh)
}

// retryForNamespace keeps one tenant namespace's Client+Worker pair alive for
// the life of the process: a failure to start or run it (that namespace not
// existing yet at pod startup, a transient network blip, ...) is logged and
// retried with exponential backoff, rather than treated as fatal — either to
// just this namespace or, as it was before this change, to the entire shared
// pool. One tenant's broken/not-yet-onboarded namespace must never take
// every other currently-served tenant down with it — this is what actually
// makes "shared pool" safe operationally, not just content-isolation-safe
// (see docs/components/multi-tenancy.md's reference-passing contract for the
// latter).
func retryForNamespace(ctx context.Context, address, namespace, taskQueue string) {
	const (
		initialBackoff = time.Second
		maxBackoff     = 30 * time.Second
	)
	backoff := initialBackoff
	for ctx.Err() == nil {
		err := runForNamespace(ctx, address, namespace, taskQueue)
		if err == nil {
			return // ctx was cancelled — clean shutdown, nothing to retry
		}
		log.Printf("loop worker for namespace %q stopped with error, retrying in %s: %v", namespace, backoff, err)
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
	taskQueue := envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop")
	nss := namespaces()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var wg sync.WaitGroup
	for _, ns := range nss {
		wg.Add(1)
		go func(namespace string) {
			defer wg.Done()
			retryForNamespace(ctx, address, namespace, taskQueue)
		}(ns)
	}

	wg.Wait()
	log.Printf("all loop workers stopped, exiting")
}
