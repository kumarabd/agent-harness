// Package ids implements the workflow-ID scheme resolved in
// components/temporal-workflow.md and components/state-layer.md: Postgres (in the
// real design) borrows these IDs verbatim rather than minting its own, and every
// ID here is a deterministic, workflow-local counter — never a random UUID — per
// the determinism constraints in components/temporal-workflow.md
// (Resolved: Determinism Constraints on the Loop).
package ids

import "fmt"

// TurnID builds a top-level turn's workflow ID: "{session_key}:turn:{turn_seq}".
func TurnID(sessionKey string, turnSeq int) string {
	return fmt.Sprintf("%s:turn:%d", sessionKey, turnSeq)
}

// SubagentTurnID builds a subagent child workflow's ID by nesting under its
// parent's turn ID: "{parent_turn_id}:sub:{n}". Recursion just keeps applying
// this — a sub-subagent extends the same parent turn ID one level further.
func SubagentTurnID(parentTurnID string, n int) string {
	return fmt.Sprintf("%s:sub:%d", parentTurnID, n)
}

// ActivityID fully qualifies a tool-call activity ID under its owning turn:
// "{turn_id}:act:{n}". Must be assigned explicitly when starting the activity
// (ExecuteActivity's ActivityID option) rather than left to the SDK's
// auto-generated ID, so Postgres can use it directly as tool_calls.tool_call_id
// in the real design (components/state-layer.md).
func ActivityID(turnID string, n int) string {
	return fmt.Sprintf("%s:act:%d", turnID, n)
}
