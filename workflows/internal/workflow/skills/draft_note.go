package skills

import (
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// DraftNoteSkillWorkflow — docs/05-architecture-domain-control-loops.md's
// harness-validation skill: not a real business process, a deliberately
// small end-to-end proof of the whole skill mechanism (discovery via
// skill_hub, minting via discover_skills, child-workflow dispatch by
// type-name string, an internally-composed approval gate, three distinct
// terminal outcomes, and a two-level cooperative-cancel cascade).
// Registered under "draft_note" in activities/activities/skills.py; must
// have a matching w.RegisterWorkflow(skillswf.DraftNoteSkillWorkflow) line in
// cmd/loop-worker/main.go.
//
// States: read its own arguments (compose) -> ask for explicit approval via
// a child UserInputRequestWorkflow, the same primitive turn.go's own
// approval-gating branch uses — direct proof a skill can compose the
// harness's existing primitives internally (docs/
// 05-architecture-domain-control-loops.md, "A domain workflow can itself
// spawn subagents or invoke further skills internally") -> record the
// outcome. Closes its own tool_calls row out itself via CloseSkillCall on
// every exit path — the same self-close-out convention
// UserInputRequestWorkflow already follows for CloseUserInput/DenyToolCall
// (user_input.go), never delegated back to whoever started it (turn.go's
// drainResult only reads the thin SkillWorkflowOutput this returns).
//
// Deliberately no context clone, no brief: SkillWorkflowInput carries only
// dispatch plumbing (IDs), independent of spawn_subagent end to end (docs/
// 05-architecture-domain-control-loops.md, "Skill Workflows Are Independent
// of Subagents").
func DraftNoteSkillWorkflow(ctx workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
	logger := workflow.GetLogger(ctx)
	out := types.SkillWorkflowOutput{ToolCallID: input.ToolCallID}

	ao := workflow.ActivityOptions{StartToCloseTimeout: activityTimeoutTierA}
	actx := workflow.WithActivityOptions(ctx, ao)

	// Compose — the skill's only way to see its real input, since
	// SkillWorkflowInput carries IDs only (reference-passing contract).
	var args map[string]any
	if err := workflow.ExecuteActivity(actx, "ReadSkillCallArguments", input.ToolCallID).Get(actx, &args); err != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "failed_to_read_arguments", "none")
		return out, nil
	}
	recipient, _ := args["recipient"].(string)
	noteText, _ := args["note_text"].(string)
	draft := "Note to " + recipient + ": " + noteText

	// Approval gate — a plain decision request, ApprovalGatedCall
	// deliberately nil: this skill decides "approved" for itself below
	// rather than asking UserInputRequestWorkflow to also dispatch a
	// wrapped ToolCall (there is no underlying tool_name to dispatch here,
	// only this skill's own next state).
	cwo := workflow.ChildWorkflowOptions{
		WorkflowID:        input.ToolCallID + ":approval",
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
	}
	cctx := workflow.WithChildOptions(ctx, cwo)
	req := types.UserInputRequest{
		RequestID: input.ToolCallID,
		TurnID:    input.TurnID,
		Kind:      "permission",
		Prompt:    "Send this note to " + recipient + "?\n\n" + draft,
		Options: []types.UserInputOption{
			{ID: "approve", Label: "Approve"},
			{ID: "deny", Label: "Deny"},
		},
		Context: map[string]any{"tool_call_id": input.ToolCallID},
	}
	fut := workflow.ExecuteChildWorkflow(cctx, wf.UserInputRequestWorkflow, types.UserInputRequestWorkflowInput{
		Request:      req,
		SessionKey:   input.SessionKey,
		ConnectionID: input.ConnectionID,
	})

	// A workflow-level error here is this skill's own ctx being cancelled
	// (an outer turn interrupt) cascading into the approval child, exactly
	// the same two-level cascade turn.go's own subagent/approval dispatch
	// already relies on (RequestCancelChildWorkflowExecution) — not a
	// distinct case to special-case, just "cancelled."
	var resp types.UserInputRequestWorkflowOutput
	if err := fut.Get(cctx, &resp); err != nil {
		logger.Info("DraftNoteSkillWorkflow: approval wait ended without a response", "tool_call_id", input.ToolCallID, "error", err)
		out.Status = closeSkillCall(ctx, input.ToolCallID, "cancelled", nil, "cancelled", "none")
		return out, nil
	}

	approved := resp.Response.SelectedOptionID != nil && *resp.Response.SelectedOptionID == "approve"
	if !approved {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "cancelled", nil, "denied_by_user", "none")
		return out, nil
	}

	// Send (approved) — no real delivery mechanism for a harness-validation
	// skill; the point is proving the mechanism, not sending a real note.
	result := map[string]any{"recipient": recipient, "note_text": noteText, "sent": true}
	out.Status = closeSkillCall(ctx, input.ToolCallID, "ok", result, "", "none")
	return out, nil
}
