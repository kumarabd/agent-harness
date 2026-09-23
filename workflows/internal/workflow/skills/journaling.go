package skills

import (
	"go.temporal.io/sdk/workflow"

	"agent-harness/workflows/internal/types"
)

// JournalingSkillWorkflow — docs/05-architecture-domain-control-loops.md.
// Records a journal entry into today's page of the user's Notion journal.
// Registered under "journaling" in activities/activities/skills.py; must
// have a matching w.RegisterWorkflow(skillswf.JournalingSkillWorkflow) line
// in cmd/loop-worker/main.go.
//
// Everything upstream of this skill — deciding something is journal-worthy
// vs. just thinking out loud, resolving what the user is referring to via
// `recall` (reference resolution only, never a source of verbatim content),
// asking clarifying questions — stays ordinary model behavior in the generic
// loop; none of that is this workflow's concern. This skill starts only once
// the model has already decided to commit an entry.
//
// The mechanical work (finding or creating the right Notion database,
// confirming with the user before writing, finding or creating today's page)
// is NOT hand-coded here — interpreting a messy real-world API response is a
// model's job, not brittle guessed parsing. This skill's only structural
// contribution is the two deterministic bookends: read the model's original
// arguments, and close out the outer tool_calls row. Everything else is
// delegated to a single scoped reasoning turn (RunReasoningTurn,
// support.go) that reuses turn.go's own reason-act loop verbatim — the model
// interacts with Notion directly via the existing discover_tools/call_tool
// path, and calls ask_user itself when it needs to confirm or disambiguate
// (already durably delivered to the user's real connection, no new plumbing).
func JournalingSkillWorkflow(ctx workflow.Context, input types.SkillWorkflowInput) (types.SkillWorkflowOutput, error) {
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

	objective := "Record this as a journal entry: " + entryText + "\n\n" +
		"Find the user's Journal database in Notion (search for one titled roughly " +
		"\"Journal\"; use discover_tools/call_tool for the real Notion tools). If none " +
		"exists, or more than one plausible match exists, use ask_user to find out " +
		"whether/where to create one, or which existing one to use. Then find today's " +
		"page in that database (or create it if this is the first entry of the day) " +
		"and append the entry text to it. Confirm with the user via ask_user, showing " +
		"them the entry text, before actually writing anything. Once written, report " +
		"what you did and finish."

	outcome, err := RunReasoningTurn(ctx, input, "reason", objective)
	if err != nil {
		out.Status = closeSkillCall(ctx, input.ToolCallID, "error", nil, "reasoning_turn_failed", "none")
		return out, nil
	}

	out.Status = closeSkillCall(ctx, input.ToolCallID, outcome.Status, map[string]any{"summary": outcome.Summary}, "", "none")
	return out, nil
}
