package skills

import (
	"strings"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// JournalingSkill — docs/05-architecture-domain-control-loops.md.
// Records a journal entry into today's page of the user's Notion journal.
// Its declared {name, description, input_schema, visibility} lives in each
// tenant's own Postgres `skills` table (activities/migrations/040_skills.sql
// — name "journaling", visibility "public", enabled by default), not in Go;
// must have a matching w.RegisterWorkflowWithOptions(skillswf.JournalingSkill,
// workflow.RegisterOptions{Name: "journaling"}) line in cmd/loop-worker/main.go.
//
// Everything upstream of this skill — deciding something is journal-worthy
// vs. just thinking out loud, resolving what the user is referring to via
// `recall` (reference resolution only, never a source of verbatim content),
// asking clarifying questions — stays ordinary model behavior in the generic
// loop; none of that is this workflow's concern. This skill starts only once
// the model has already decided to commit an entry.
//
// The first native state is an explicit confirmation gate. It deliberately
// uses the harness's existing durable UserInputRequestWorkflow directly,
// rather than asking a nested generic reasoning loop to remember to do it.
// Dynamic Notion capability discovery and interpretation remain on the legacy
// scoped-reasoning bridge for now; those are the next states to extract once
// the state-oriented capability invocation primitive lands. That bridge keeps
// the skill useful while this workflow is migrated incrementally instead of
// replacing a real integration with guessed Notion API parsing.
func JournalingSkill(ctx workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
	ctx = wf.WithTenantTaskQueue(ctx, input.TenantSlug)
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
	entryText, _ := args["entry_text"].(string)
	entryText = strings.TrimSpace(entryText)
	if entryText == "" {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "entry_text_is_required", "none")
		return out, nil
	}

	// Native skill state: the write cannot proceed until the person confirms
	// the exact entry. A direct child UserInputRequestWorkflow preserves its
	// durable wait, delivery, timeout, cancellation, and response-routing
	// behavior without involving TurnWorkflow or RunReasonActLoop.
	confirmID := input.ToolCallID + ":journal-confirm"
	confirmCtx := workflow.WithChildOptions(ctx, workflow.ChildWorkflowOptions{
		WorkflowID:        confirmID,
		ParentClosePolicy: enumspb.PARENT_CLOSE_POLICY_REQUEST_CANCEL,
	})
	var confirmation types.UserInputRequestWorkflowOutput
	confirmErr := workflow.ExecuteChildWorkflow(confirmCtx, wf.UserInputRequestWorkflow, types.UserInputRequestWorkflowInput{
		Request: types.UserInputRequest{
			RequestID: confirmID,
			TurnID:    input.TurnID,
			Kind:      "decision",
			Prompt:    "Record this in your journal?\n\n" + entryText,
			Options: []types.UserInputOption{
				{ID: "approve", Label: "Record it"},
				{ID: "decline", Label: "Do not record it"},
			},
			AllowFreeText: true,
			Context:       map[string]any{"skill": "journaling", "state": "confirm_entry"},
		},
		SessionKey:   input.SessionKey,
		ConnectionID: input.ConnectionID,
		TenantSlug:   input.TenantSlug,
	}).Get(confirmCtx, &confirmation)
	if confirmErr != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "cancelled", nil, "confirmation_cancelled", "none")
		return out, nil
	}
	if confirmation.Response.SelectedOptionID == nil || *confirmation.Response.SelectedOptionID != "approve" {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "cancelled", nil, "entry_not_confirmed", "none")
		return out, nil
	}

	objective := "Record this as a journal entry: " + entryText + "\n\n" +
		"The user has explicitly approved this exact entry. Do not ask for confirmation again. " +
		"Write in a first-person diary voice that preserves the user's wording, perspective, " +
		"and uncertainty; do not turn it into a generic summary. If essential context is missing " +
		"or a reference is ambiguous, use recall before asking the user a focused clarification. " +
		"Use Notion as a diary hierarchy, not a database. Find the page titled \"My Diary\" " +
		"and treat it as the diary's root. If it does not exist, or more than one plausible " +
		"page exists, use ask_user to decide whether/where to create it or which one to use. " +
		"Under that root, find the child page named for the user's current local calendar date " +
		"in YYYY-MM-DD format (for example, 2026-09-27). Create that dated child page if this " +
		"is the first entry for the day, then append the entry text there. Once written, verify " +
		"the result, report what you did, and finish."

	outcome, err := RunReasoningTurn(ctx, input, "reason", objective)
	if err != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "reasoning_turn_failed", "none")
		return out, nil
	}

	out.Status = closeSkillCall(ctx, input.ToolCallID, outcome.Status, map[string]any{"summary": outcome.Summary}, "", "none")
	return out, nil
}
