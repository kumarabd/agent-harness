package activities

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// CreateK8sNamespace shells out to kubectl (exec.go's runCommand) — this
// worker's own ServiceAccount holds the ClusterRole permission for
// "namespaces" create/get (deploy/helm/agent-harness-shared/templates/
// automation-rbac.yaml), scoped to only this worker, never the shared
// loop-worker/router's own ServiceAccount. `kubectl create namespace
// --dry-run=client -o yaml | kubectl apply -f -` is the idempotent form —
// safe to run again on a workflow retry, unlike a bare `kubectl create`
// which would fail on a namespace this same workflow already created in an
// earlier attempt.
func (a *Activities) CreateK8sNamespace(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		manifest, err := runCommand(ctx, "kubectl", "create", "namespace", ref.TenantSlug, "--dry-run=client", "-o", "yaml")
		if err != nil {
			return fmt.Errorf("render namespace manifest: %w", err)
		}
		if _, err := runCommandStdin(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
			return fmt.Errorf("apply namespace: %w", err)
		}
		return nil
	})
}

// HealthCheck polls the newly-installed tenant's Gateway and agent-brain
// Services (cross-namespace, same DNS names workflows/internal/router/
// registry.Tenant computes) until both answer, before the workflow marks
// onboarding complete — a real user landing on a "your workspace is ready"
// screen (docs/components/gateway/web.md's Phase 3) should mean it
// actually is. Bounded retry loop, not activity.RetryPolicy, since a
// "service not up yet" response during normal pod startup is expected and
// routine here, not a failure to surface through Temporal's own retry/
// backoff machinery.
func (a *Activities) HealthCheck(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		gatewayURL := fmt.Sprintf("http://%s-gateway.%s.svc.cluster.local:%d/healthz", ref.TenantSlug, ref.TenantSlug, a.gatewayPort())
		brainURL := fmt.Sprintf("http://%s-memory-server.%s.svc.cluster.local:%d/healthz", ref.TenantSlug, ref.TenantSlug, a.agentBrainPort())

		client := &http.Client{Timeout: 5 * time.Second}
		deadline := time.Now().Add(5 * time.Minute)
		for _, url := range []string{gatewayURL, brainURL} {
			for {
				ok, err := probeOnce(ctx, client, url)
				if ok {
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("timed out waiting for %s to become healthy: %v", url, err)
				}
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil
	})
}

// probeOnce reports whether url answered 2xx. The returned error is always
// non-nil when ok is false (a transport error or a non-2xx status), purely
// for HealthCheck's own timeout message — callers must check ok, not err,
// to decide success.
func probeOnce(ctx context.Context, client *http.Client, url string) (ok bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	res, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		return true, nil
	}
	return false, fmt.Errorf("status %d", res.StatusCode)
}

func (a *Activities) gatewayPort() int {
	if a.GatewayPort > 0 {
		return a.GatewayPort
	}
	return 8090
}

func (a *Activities) agentBrainPort() int {
	if a.AgentBrainPort > 0 {
		return a.AgentBrainPort
	}
	return 8080
}
