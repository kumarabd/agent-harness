package skills

import "encoding/json"

// Registry mirrors activities/activities/skills.py's SKILLS list by hand —
// name/description/input_schema only, the same Go/Python identity split
// tool_tiers.go already accepts for tools.py's TOOL_REGISTRY
// (docs/components/tool-registry.md): the workflow layer can't ask the
// activity layer for this at runtime, so this chart of what skills.py
// exports is a second, separate hand-sync point from the workflow-type
// registrations in cmd/loop-worker/main.go — keep both in step with
// skills.py, not just one.
//
// This exists purely to serve the gateway's own GET /skills — the model's
// own runtime discovery (discover_skills, skill_hub.py's zvec index) reads
// skills.py directly and has no use for this. Go can't just import a Python
// module, hence the mirror.
type Skill struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

var Registry = []Skill{
	{
		Name: "journaling",
		Description: "Record a journal entry the user has decided to keep, into today's page in " +
			"their Notion journal. Only call this once the user has confirmed something is " +
			"actually journal material (not just thinking out loud) — this skill itself " +
			"runs its own reasoning turn to find or create the right Notion database, " +
			"confirm the entry with the user, and write it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"entry_text": {"type": "string", "description": "The journal entry's content."}
			},
			"required": ["entry_text"]
		}`),
	},
	{
		Name: "service_monitoring",
		Description: "Configure durable monitoring for a Kubernetes service. This skill uses Grafana as the " +
			"mandatory source of monitoring evidence: it finds the relevant Grafana capability, inspects " +
			"the service's actual metrics, dashboards, and existing alerts, selects an appropriate health " +
			"signal, asks before making consequential external changes, and arms a monitoring intention. " +
			"Use this when the user asks to watch, monitor, or be alerted about a Kubernetes service.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"service": {"type": "string", "description": "Kubernetes service or workload to monitor."},
				"namespace": {"type": "string", "description": "Optional Kubernetes namespace containing the service."},
				"cluster": {"type": "string", "description": "Optional cluster name or identifier."},
				"notify_when": {"type": "string", "description": "What should trigger notification; defaults to the service being unavailable."}
			},
			"required": ["service"]
		}`),
	},
}
