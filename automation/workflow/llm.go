package workflow

import (
	"go.temporal.io/sdk/workflow"

	"agent-harness/automation/activities"
)

// TenantLLMUpdateWorkflow applies a post-onboarding edit of a tenant's LLM
// tiers (helm upgrade, then the same HealthCheck onboarding uses). Progress is
// queryable via ProgressQuery, same shape as onboarding.
func TenantLLMUpdateWorkflow(ctx workflow.Context, in activities.LLMUpdateInput) error {
	var a *activities.Activities
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())
	ref := in.Ref()

	progress := Progress{Status: "running"}
	if err := workflow.SetQueryHandler(ctx, ProgressQuery, func() (Progress, error) { return progress, nil }); err != nil {
		return err
	}
	step := func(name string, fn func() error) error {
		progress.Steps = append(progress.Steps, StepProgress{Step: name, Status: "running"})
		i := len(progress.Steps) - 1
		if err := fn(); err != nil {
			progress.Steps[i] = StepProgress{Step: name, Status: "failed", Message: err.Error()}
			progress.Status, progress.Error = "failed", err.Error()
			return err
		}
		progress.Steps[i].Status = "done"
		return nil
	}

	if err := step("HelmUpdateLLM", func() error {
		return workflow.ExecuteActivity(ctx, a.HelmUpdateLLM, in).Get(ctx, nil)
	}); err != nil {
		return err
	}
	hc := workflow.WithActivityOptions(ctx, healthCheckActivityOptions())
	if err := step("HealthCheck", func() error {
		return workflow.ExecuteActivity(hc, a.HealthCheck, ref).Get(hc, nil)
	}); err != nil {
		return err
	}
	progress.Status = "completed"
	return nil
}

// TenantLLMReadWorkflow returns the tenant's current tiers without keys — a
// workflow only because the router's one route to the cluster is Temporal.
func TenantLLMReadWorkflow(ctx workflow.Context, ref activities.PublicRef) (map[string]activities.LLMTierView, error) {
	var a *activities.Activities
	ctx = workflow.WithActivityOptions(ctx, defaultActivityOptions())
	var out map[string]activities.LLMTierView
	err := workflow.ExecuteActivity(ctx, a.GetTenantLLM, ref).Get(ctx, &out)
	return out, err
}
