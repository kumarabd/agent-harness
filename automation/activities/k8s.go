package activities

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.temporal.io/sdk/activity"
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
	manifest, err := runCommand(ctx, "kubectl", "create", "namespace", ref.TenantSlug, "--dry-run=client", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("render namespace manifest: %w", err)
	}
	if _, err := runCommandStdin(ctx, manifest, "kubectl", "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply namespace: %w", err)
	}
	return nil
}

// HealthCheck polls the newly-installed tenant's Gateway and agent-brain
// Services (cross-namespace, bare Service names — 2026-09-27: the tenant
// chart dropped its release-name/tenant-slug prefix, deploy/helm/
// agent-harness-tenant/values.yaml, since every tenant gets its own
// dedicated namespace; router/internal/core/tenant.go's own
// GatewayBaseURL/AgentBrainBaseURL were updated to match at the same time —
// this was the one other copy of that same DNS convention, missed in that
// pass, which meant every self-serve onboarding's HealthCheck was probing a
// Service name that no longer existed) until both answer, before the
// workflow marks onboarding complete — a real user landing on a "your
// workspace is ready" screen (docs/components/gateway/web.md's Phase 3)
// should mean it actually is. Bounded retry loop, not activity.RetryPolicy,
// since a "service not up yet" response during normal pod startup is
// expected and routine here, not a failure to surface through Temporal's
// own retry/backoff machinery.
//
// activity.RecordHeartbeat every iteration — this loop can legitimately run
// for minutes (a pod pulling an image or waiting on a migration Job), and
// workflow/onboarding.go gives HealthCheck its own generous HeartbeatTimeout
// specifically so that's fine, but only as long as something actually calls
// RecordHeartbeat inside it. Without it, Temporal has no way to tell "still
// working" from "stuck" and kills the activity as heartbeat-timed-out the
// moment that timeout elapses, regardless of how much of the 5-minute
// deadline below is actually left.
func (a *Activities) HealthCheck(ctx context.Context, ref PublicRef) error {
	gatewayURL := fmt.Sprintf("http://gateway.%s.svc.cluster.local:%d/healthz", ref.TenantSlug, a.gatewayPort())
	brainURL := fmt.Sprintf("http://memory-server.%s.svc.cluster.local:%d/healthz", ref.TenantSlug, a.agentBrainPort())

	client := &http.Client{Timeout: 5 * time.Second}
	deadline := time.Now().Add(5 * time.Minute)
	for _, url := range []string{gatewayURL, brainURL} {
		for {
			activity.RecordHeartbeat(ctx, fmt.Sprintf("probing %s", url))
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
