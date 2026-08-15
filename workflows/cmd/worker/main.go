// Command worker registers the Session Coordinator and Turn Workflow on the
// configured task queue and polls a Temporal server for work. Run alongside
// the Python activity worker (activities/activities/worker.py), which polls
// the same task queue for the ModelCall/ToolCall/InsertMessage/Persist/Deliver/
// CompressContext activities referenced by name from the workflows here.
//
// Configured via env vars (not hardcoded) so this binary is deployable —
// see deploy/docker/workflow-worker.Dockerfile and deploy/helm/agent-harness:
//
//	TEMPORAL_ADDRESS    Temporal frontend host:port. Default: localhost:7233.
//	TEMPORAL_NAMESPACE  Single Temporal namespace. Default: default. Used only
//	                    if TEMPORAL_NAMESPACES is unset — backward-compat with
//	                    single-tenant local-dev usage.
//	TEMPORAL_NAMESPACES Comma-separated list of Temporal namespaces this
//	                    process serves — one Client+Worker pair per namespace,
//	                    run concurrently in this single process
//	                    (docs/components/multi-tenancy.md, "Resolved: Compute
//	                    Isolation" — the shared, tenant-agnostic workflow-worker
//	                    pool). Static config, chosen deliberately over dynamic
//	                    registration, mirroring the gateway's static-shard-config
//	                    pattern (components/gateway.md). Takes precedence over
//	                    TEMPORAL_NAMESPACE if both are set.
//	TEMPORAL_TASK_QUEUE Task queue name, shared across every namespace this
//	                    process serves. Default: agent-loop. Must match the
//	                    Python activity worker's TEMPORAL_TASK_QUEUE for each
//	                    corresponding tenant's activity-worker fleet.
package main

import (
	"log"
	"os"
	"strings"
	"sync"

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
// and blocks until it exits (cleanly via interrupt, or with an error).
func runForNamespace(address, namespace, taskQueue string) error {
	c, err := client.Dial(client.Options{HostPort: address, Namespace: namespace})
	if err != nil {
		return err
	}
	defer c.Close()

	// LocalActivityWorkerOnly: this process registers no activities — every
	// activity (ModelCall, ToolCall, InsertMessage, Persist, Deliver,
	// CompressContext) is implemented by that tenant's Python activity worker
	// (activities/activities/worker.py). Without this flag, this worker would
	// also poll for regular activity tasks on the same queue and occasionally
	// win that race, failing the task since it has no implementation
	// registered.
	w := worker.New(c, taskQueue, worker.Options{LocalActivityWorkerOnly: true})
	w.RegisterWorkflow(wf.CoordinatorWorkflow)
	w.RegisterWorkflow(wf.TurnWorkflow)

	log.Printf("workflow worker starting: temporal=%q namespace=%q task_queue=%q", address, namespace, taskQueue)
	return w.Run(worker.InterruptCh())
}

func main() {
	address := envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort)
	taskQueue := envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop")
	nss := namespaces()

	if len(nss) == 1 {
		// Common case (single tenant / local dev): run inline, no goroutine
		// fan-out needed, and a failure here is the process's own failure —
		// same behavior as before this change.
		if err := runForNamespace(address, nss[0], taskQueue); err != nil {
			log.Fatalf("worker stopped with error: %v", err)
		}
		return
	}

	// Shared pool serving multiple tenant namespaces: one Client+Worker pair
	// per namespace, running concurrently in this process
	// (docs/components/multi-tenancy.md). Simplest correct behavior for this
	// pass: if any pair's Run() returns an error, the whole process exits —
	// graceful partial-degradation (keep serving the healthy namespaces,
	// report the unhealthy one) is a real future concern not designed in the
	// docs, so not invented here.
	var wg sync.WaitGroup
	errCh := make(chan error, len(nss))
	for _, ns := range nss {
		wg.Add(1)
		go func(namespace string) {
			defer wg.Done()
			if err := runForNamespace(address, namespace, taskQueue); err != nil {
				errCh <- err
			}
		}(ns)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			log.Fatalf("worker stopped with error: %v", err)
		}
	}
}
