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
// running automatically — docs/components/gateway/web.md's Phase 2 section
// flags this explicitly as a deliberate, reviewable choice, not an
// oversight.
//
//	temporal workflow signal --workflow-id tenant-onboard:<slug> \
//	  --name approve-shared-pool-rollout --namespace system
const ApproveSharedPoolRolloutSignal = "approve-shared-pool-rollout"

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

	markStatus := func(status, errMsg string) {
		// Best-effort: a failure to record status is not itself grounds to
		// fail an otherwise-successful (or already-failing) workflow.
		_ = workflow.ExecuteActivity(ctx, a.MarkRequestStatus, ref, status, errMsg).Get(ctx, nil)
	}
	fail := func(err error) (activities.TenantOnboardingResult, error) {
		markStatus("failed", err.Error())
		return result, err
	}

	markStatus("running", "")

	if err := workflow.ExecuteActivity(ctx, a.ValidateRequest, in).Get(ctx, nil); err != nil {
		return fail(err)
	}
	if err := workflow.ExecuteActivity(ctx, a.RegisterTemporalNamespace, ref).Get(ctx, nil); err != nil {
		return fail(err)
	}
	if err := workflow.ExecuteActivity(ctx, a.CreateK8sNamespace, ref).Get(ctx, nil); err != nil {
		return fail(err)
	}
	if err := workflow.ExecuteActivity(ctx, a.StageTenantSecrets, in).Get(ctx, nil); err != nil {
		return fail(err)
	}

	// From here on, every failure path must still clean up the staged
	// secret — a disconnected context so cleanup runs even if ctx itself
	// was cancelled (e.g. this workflow's own execution being terminated),
	// same pattern discord.go's own connection teardown uses.
	cleanupCtx, cancelCleanup := workflow.NewDisconnectedContext(ctx)
	cleanupCtx = workflow.WithActivityOptions(cleanupCtx, defaultActivityOptions())
	cleanup := func() {
		_ = workflow.ExecuteActivity(cleanupCtx, a.CleanupStagedSecret, ref).Get(cleanupCtx, nil)
		cancelCleanup()
	}

	if err := workflow.ExecuteActivity(ctx, a.HelmInstallTenant, ref).Get(ctx, nil); err != nil {
		cleanup()
		return fail(err)
	}

	markStatus("awaiting_approval", "")
	workflow.GetSignalChannel(ctx, ApproveSharedPoolRolloutSignal).Receive(ctx, nil)
	markStatus("running", "")

	if err := workflow.ExecuteActivity(ctx, a.RegisterSharedPoolNamespace, ref).Get(ctx, nil); err != nil {
		cleanup()
		return fail(err)
	}

	var orgID string
	if err := workflow.ExecuteActivity(ctx, a.CreateClerkOrganization, ref).Get(ctx, &orgID); err != nil {
		cleanup()
		return fail(err)
	}
	if err := workflow.ExecuteActivity(ctx, a.RegisterTenantInRegistry, ref, orgID).Get(ctx, nil); err != nil {
		cleanup()
		return fail(err)
	}
	if err := workflow.ExecuteActivity(ctx, a.HealthCheck, ref).Get(ctx, nil); err != nil {
		cleanup()
		return fail(err)
	}

	cleanup()
	markStatus("completed", "")
	return result, nil
}
