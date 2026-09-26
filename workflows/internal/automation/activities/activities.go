package activities

import (
	"context"
	"fmt"

	"go.temporal.io/sdk/activity"

	"agent-harness/workflows/internal/onboarding"
	"agent-harness/workflows/internal/router/registry"
)

// Activities holds every dependency this package's activity methods need —
// a single struct registered once with the worker (workflows/cmd/automation/
// main.go), matching the "composition root builds shared infra, wires it
// into one place" shape every other cmd/ binary in this repo already uses.
type Activities struct {
	Store    *onboarding.Store
	Registry *registry.Registry

	TemporalAddress        string // dialed fresh per RegisterTemporalNamespace call — see that file's own comment on why a NamespaceClient isn't reused
	NamespaceRetentionDays int

	ChartDir        string // deploy/helm/agent-harness-tenant, baked into this image — see helm.go
	SharedChartDir  string // deploy/helm/agent-harness-shared, baked into this image
	SharedRelease   string // the shared chart's own release name (e.g. "harness")
	SharedNamespace string // k8s namespace the shared release lives in

	ClerkIssuer     string // the one shared Clerk issuer (Phase 1's single-project migration) — written into every generated tenant's gateway.web.clerkIssuer
	ClerkSecretKey  string // Clerk's Backend API — see clerk.go's own comment; the ONE place in this whole codebase that holds it
	ClerkAPIBaseURL string // override for tests; defaults to https://api.clerk.com

	GatewayPort    int // matches agent-harness-tenant/values.yaml's gateway.port default (8090) — written into tenant_registry
	AgentBrainPort int // matches that chart's agent-brain subchart default (8080)
}

// MarkRequestStatus updates tenant_onboarding_requests.status directly —
// the workflow's own coarse-grained status (pending/running/
// awaiting_approval/completed/failed), distinct from the fine-grained
// per-step rows runStep writes to tenant_onboarding_steps. Not wrapped in
// runStep itself: a single Postgres UPDATE has no meaningful "running"
// phase worth recording as its own step.
func (a *Activities) MarkRequestStatus(ctx context.Context, ref PublicRef, status, errMsg string) error {
	return a.Store.SetRequestStatus(ctx, ref.RequestID, status, errMsg)
}

// runStep is the shared start/success/failure wrapper every activity method
// in this package calls itself with — records a "running" row before fn
// runs and a "done"/"failed" row after, exactly the "activities write
// progress rows a poller re-reads" shape described in
// docs/components/gateway/web.md's Phase 2 section. activity.GetInfo's
// ActivityType.Name is used as the step name so the recorded step always
// matches the Go method that actually ran, not a separately-maintained
// string.
func runStep(ctx context.Context, store *onboarding.Store, requestID string, fn func(context.Context) error) error {
	step := activity.GetInfo(ctx).ActivityType.Name
	if err := store.RecordStep(ctx, requestID, step, onboarding.StepStatusRunning, ""); err != nil {
		return fmt.Errorf("record step running: %w", err)
	}
	if err := fn(ctx); err != nil {
		_ = store.RecordStep(ctx, requestID, step, onboarding.StepStatusFailed, err.Error())
		return err
	}
	if err := store.RecordStep(ctx, requestID, step, onboarding.StepStatusDone, ""); err != nil {
		return fmt.Errorf("record step done: %w", err)
	}
	return nil
}
