// Package ids implements the workflow-ID scheme resolved in
// components/temporal-workflow.md and components/state-layer.md: Postgres (in the
// real design) borrows these IDs verbatim rather than minting its own, and every
// ID here is a deterministic, workflow-local counter — never a random UUID — per
// the determinism constraints in components/temporal-workflow.md
// (Resolved: Determinism Constraints on the Loop).
package ids

import (
	"fmt"
	"strings"
)

// UserScopeOf returns the user-stable scope a standing wake keys on
// (docs/components/proactivity.md). A session_key is deliberately
// channel/branch-scoped, not user-scoped (gateway core.SessionKeyFor) — for web
// it embeds the user ("agent:main:web:user:<id>"); for a shared Discord channel
// the channel is the best available scope. Stripping any per-branch
// ":session:<id>" or per-thread ":thread:<id>" suffix gives a namespace shared
// across a user's branches/threads, and the result is always itself a valid
// canonical session_key — so it also names the session a fired wake wakes.
// Mirrored in tenant-worker/tenant_worker/ids.py (user_scope_of).
func UserScopeOf(sessionKey string) string {
	for _, marker := range []string{":session:", ":thread:"} {
		if head, _, ok := strings.Cut(sessionKey, marker); ok {
			return head
		}
	}
	return sessionKey
}

// WakeRef is what a wake records as its own identity: "{scope}:{slug}", where
// scope is UserScopeOf(session_key). A fired wake stamps its turn's
// initiated_by as "wake:" + WakeRef. Mirrored in tools_wake.py.
func WakeRef(scope, slug string) string {
	return fmt.Sprintf("%s:%s", scope, slug)
}

// WakeScheduleID builds a time wake's Temporal Schedule id:
// "wake:{scope}:{slug}". The Schedule IS the record — listed, paused, retimed
// and cancelled through Temporal's own schedule APIs — so there is no wakes
// table to keep in sync with it.
func WakeScheduleID(scope, slug string) string {
	return "wake:" + WakeRef(scope, slug)
}

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
