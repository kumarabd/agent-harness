// Package workflow implements TenantOnboardingWorkflow — the self-serve
// tenant provisioning workflow docs/components/gateway/web.md's Phase 2
// describes. Runs on the "system" Temporal namespace's own task queue
// (workflows/cmd/automation/main.go), separate from every tenant's own
// "agent-loop" queue — a runaway onboarding can never starve real turn
// traffic, and vice versa.
package workflow

import (
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/automation/activities"
)

// ApproveSharedPoolRolloutSignal is the human-approval gate before
// RegisterSharedPoolNamespace — the one step in this workflow that mutates
// the single, cluster-wide agent-harness-shared release every OTHER
// tenant's turns already depend on (that chart's own values.yaml comment:
// "this rolls the whole shared pool"). Every other step here is safely
// scoped to just the new tenant's own namespace; this one alone touches
// shared state, so it waits for an operator to send this signal rather than
// running automatically.
//
//	temporal workflow signal --workflow-id tenant-onboard:<slug> \
//	  --name approve-shared-pool-rollout --namespace system
const ApproveSharedPoolRolloutSignal = "approve-shared-pool-rollout"

// ProgressQuery is how the router's GET /onboard/{request_id}
// (workflows/internal/router/core/onboarding.go) reads live status — a
// Temporal Query against this workflow directly, not a database read. There
// is no Postgres anywhere in this system as of 2026-09-25: tenant identity
// is pure convention, and progress lives in the workflow's own state.
const ProgressQuery = "progress"

type StepProgress struct {
	Step    string `json:"step"`
	Status  string `json:"status"` // running|done|failed
	Message string `json:"message,omitempty"`
}

type Progress struct {
	Status string         `json:"status"` // pending|running|awaiting_approval|completed|failed
	Error  string         `json:"error,omitempty"`
	Steps  []StepProgress `json:"steps"`
}

// defaultActivityOptions applies to every activity below except HealthCheck
// (which runs its own bounded polling loop internally, see activities/
// k8s.go) — a real `helm upgrade --install` pulling three chart
// dependencies can legitimately take minutes, so this is generous on
// purpose; heartbeating (exec.go) is what actually detects a truly stuck
// subprocess, not this timeout.
func defaultActivityOptions() workflow.ActivityOptions {
	return workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		HeartbeatTimeout:    30 * time.Second,
		RetryPolicy:         &temporal.RetryPolicy{MaximumAttempts: 3},
	}
}

func TenantOnboardingWorkflow(ctx workflow.Context, in activities.TenantOnboardingInput) (activities.TenantOnboardingResult, error) {
	var a *activities.Activities // nil — only used for its method values' types; the real instance lives on the activity worker (workflows/cmd/automation/main.go)

	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())
	ref := in.Ref()
	result := activities.TenantOnboardingResult{TenantSlug: ref.TenantSlug, Namespace: ref.TenantSlug}

	progress := Progress{Status: "pending"}
	if err := workflow.SetQueryHandler(ctx, ProgressQuery, func() (Progress, error) { return progress, nil }); err != nil {
		return result, err
	}

	// runStep records a step's running/done/failed transition in the
	// workflow's own local state, queryable live via ProgressQuery — the
	// direct replacement for the old Postgres-backed runStep wrapper that
	// used to live in the activities package.
	runStep := func(name string, fn func() error) error {
		progress.Steps = append(progress.Steps, StepProgress{Step: name, Status: "running"})
		idx := len(progress.Steps) - 1
		if err := fn(); err != nil {
			progress.Steps[idx].Status = "failed"
			progress.Steps[idx].Message = err.Error()
			return err
		}
		progress.Steps[idx].Status = "done"
		return nil
	}
	fail := func(err error) (activities.TenantOnboardingResult, error) {
		progress.Status = "failed"
		progress.Error = err.Error()
		return result, err
	}

	progress.Status = "running"

	if err := runStep("ValidateRequest", func() error {
		return workflow.ExecuteActivity(ctx, a.ValidateRequest, in).Get(ctx, nil)
	}); err != nil {
		return fail(err)
	}
	if err := runStep("RegisterTemporalNamespace", func() error {
		return workflow.ExecuteActivity(ctx, a.RegisterTemporalNamespace, ref).Get(ctx, nil)
	}); err != nil {
		return fail(err)
	}
	if err := runStep("CreateK8sNamespace", func() error {
		return workflow.ExecuteActivity(ctx, a.CreateK8sNamespace, ref).Get(ctx, nil)
	}); err != nil {
		return fail(err)
	}
	if err := runStep("StageTenantSecrets", func() error {
		return workflow.ExecuteActivity(ctx, a.StageTenantSecrets, in).Get(ctx, nil)
	}); err != nil {
		return fail(err)
	}

	// From here on, every failure path must still clean up the staged
	// secret — a disconnected context so cleanup runs even if ctx itself
	// was cancelled (e.g. this workflow's own execution being terminated).
	cleanupCtx, cancelCleanup := workflow.NewDisconnectedContext(ctx)
	cleanupCtx = workflow.WithActivityOptions(cleanupCtx, defaultActivityOptions())
	cleanup := func() {
		_ = runStep("CleanupStagedSecret", func() error {
			return workflow.ExecuteActivity(cleanupCtx, a.CleanupStagedSecret, ref).Get(cleanupCtx, nil)
		})
		cancelCleanup()
	}

	if err := runStep("HelmInstallTenant", func() error {
		return workflow.ExecuteActivity(ctx, a.HelmInstallTenant, ref).Get(ctx, nil)
	}); err != nil {
		cleanup()
		return fail(err)
	}

	progress.Status = "awaiting_approval"
	workflow.GetSignalChannel(ctx, ApproveSharedPoolRolloutSignal).Receive(ctx, nil)
	progress.Status = "running"

	if err := runStep("RegisterSharedPoolNamespace", func() error {
		return workflow.ExecuteActivity(ctx, a.RegisterSharedPoolNamespace, ref).Get(ctx, nil)
	}); err != nil {
		cleanup()
		return fail(err)
	}

	// No Clerk Organization to create and no tenant_registry to write —
	// tenant identity is pure convention (workflows/internal/router/core/
	// tenant.go): the router already resolves ref.RequesterUserID to this
	// exact ref.TenantSlug the instant a request comes in, whether or not
	// this workflow has finished. Provisioning the actual infrastructure at
	// that same slug (already done, above) is the only thing that was ever
	// missing.

	if err := runStep("HealthCheck", func() error {
		return workflow.ExecuteActivity(ctx, a.HealthCheck, ref).Get(ctx, nil)
	}); err != nil {
		cleanup()
		return fail(err)
	}

	cleanup()
	progress.Status = "completed"
	return result, nil
}
