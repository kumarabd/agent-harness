package skills

import (
	"strings"

	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// ServiceMonitoringSkill configures a durable monitoring commitment for one
// Kubernetes service. Grafana is deliberately mandatory: it is the source of
// monitoring evidence used to select a health signal, never a best-effort
// alternative to another telemetry source. The skill does not hand-code a
// Grafana API contract; its scoped reasoning turn discovers and uses the
// connected Grafana MCP capability so it can interpret the deployment's real
// dashboards, metrics, and existing alerts.
//
// It is registered under "service_monitoring" in skills.py. Like the other
// skills, it owns only the process bookends: read its real arguments and close
// its outer tool call. The shared RunReasoningTurn supplies the same durable
// reason-act loop, tool discovery, approval requests, and generic intention
// creation that an ordinary turn uses.
func ServiceMonitoringSkill(ctx workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
	ctx = wf.WithTenantTaskQueue(ctx, input.TenantSlug)
	out := types.SkillWorkflowOutput{ToolCallID: input.ToolCallID}

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)
	var args map[string]any
	if err := workflow.ExecuteActivity(actx, "ReadSkillCallArguments", input.ToolCallID).Get(actx, &args); err != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "failed_to_read_arguments", "none")
		return out, nil
	}

	service, _ := args["service"].(string)
	service = strings.TrimSpace(service)
	if service == "" {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "service_is_required", "none")
		return out, nil
	}
	namespace, _ := args["namespace"].(string)
	cluster, _ := args["cluster"].(string)
	notifyWhen, _ := args["notify_when"].(string)
	if strings.TrimSpace(notifyWhen) == "" {
		notifyWhen = "the service is unavailable"
	}

	target := "service " + service
	if strings.TrimSpace(namespace) != "" {
		target += " in namespace " + namespace
	}
	if strings.TrimSpace(cluster) != "" {
		target += " on cluster " + cluster
	}

	objective := "Configure durable monitoring for Kubernetes " + target + ". The requested " +
		"notification condition is: " + notifyWhen + ".\n\n" +
		"Grafana is mandatory for this task. First call discover_tools specifically for Grafana, then use " +
		"a discovered Grafana capability to inspect the real monitoring details for this target: relevant " +
		"dashboards, metrics, queries, and any existing alerts. Do not substitute Kubernetes, Prometheus, " +
		"or another source as the monitoring evidence. If Grafana is unavailable, inaccessible, or cannot " +
		"establish a trustworthy signal for this target, do not create an intention or an alert; explain the " +
		"blocker and finish.\n\n" +
		"Use what Grafana shows to select the least-surprising health signal and determine whether an existing " +
		"Grafana alert can be used, a Grafana query can be polled, or a Grafana alert rule must be created. " +
		"Before creating, editing, enabling, or otherwise changing any external alerting configuration, ask the " +
		"user for explicit approval and state the proposed signal and threshold. After approval, make only the " +
		"approved change. Then create the generic intention that implements the resulting monitoring commitment. " +
		"For a polling intention, use the resolved Grafana server/tool identity and a concrete predicate. Remember " +
		"that the current generic poll intention fires once; if durable repeat monitoring requires a recurring " +
		"review rather than a one-shot condition, state that choice clearly. Finish by summarizing the Grafana " +
		"evidence, selected mechanism, and the armed intention or blocker."

	outcome, err := RunReasoningTurn(ctx, input, "reason", objective)
	if err != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "reasoning_turn_failed", "none")
		return out, nil
	}

	out.Status = closeSkillCall(ctx, input.ToolCallID, outcome.Status, map[string]any{"summary": outcome.Summary}, "", "none")
	return out, nil
}
