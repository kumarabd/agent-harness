package workflow

import "time"

// toolTiming is the per-tool activity tuning the Turn Workflow needs before
// calling ExecuteActivity for a ToolCall — mirrors
// activities/activities/tools.py's TOOL_REGISTRY by hand (same
// hand-mirrored-across-languages pattern already used for
// types.go/types.py and ids.go/ids.py; the workflow can't ask the Python
// activity layer for this at runtime, since it has to be set on
// ActivityOptions *before* the activity is dispatched).
type toolTiming struct {
	HeartbeatTimeout    time.Duration
	StartToCloseTimeout time.Duration
}

// toolActivityOptions holds a real entry only for tools with a real
// implementation (docs/components/activities-outbound-delivery.md's Tier
// A/B/C heartbeat policy, applied for real). Any tool name absent here —
// including every tool the existing fixture-driven scenarios use (search,
// slow_tool, noop_tool; see workflows/scenarios/), none of which are real
// tools — falls back to defaultToolTiming, preserving this project's
// existing fast local-demo timing exactly.
var toolActivityOptions = map[string]toolTiming{
	// shell_exec: Tier B — heartbeat ~3s (Python side), ~10s heartbeat
	// timeout, 5-minute ceiling on total run time.
	"shell_exec": {
		HeartbeatTimeout:    10 * time.Second,
		StartToCloseTimeout: 5 * time.Minute,
	},
	// merge_subagent_output: Tier B, same tuning as shell_exec — also
	// filesystem-touching, chunkable per-file work on the same PV, holds a
	// session-directory lease the same way, needs the same heartbeat cadence
	// for real cancellation delivery mid-merge. See
	// docs/components/session-filesystem.md, "Resolved: Subagent Merge-Back
	// Mechanics."
	"merge_subagent_output": {
		HeartbeatTimeout:    10 * time.Second,
		StartToCloseTimeout: 5 * time.Minute,
	},
}

// defaultToolTiming is today's existing local-demo tuning (see the comment
// this replaced in turn.go), preserved verbatim as the fallback.
var defaultToolTiming = toolTiming{
	HeartbeatTimeout:    1 * time.Second,
	StartToCloseTimeout: activityTimeoutTierA,
}

func toolTimingFor(toolName string) toolTiming {
	if t, ok := toolActivityOptions[toolName]; ok {
		return t
	}
	return defaultToolTiming
}
