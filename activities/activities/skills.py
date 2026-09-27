"""skills.py — docs/05-architecture-domain-control-loops.md, docs/components/
turn-pipeline.md ("Skills"). The hand-maintained registry of authored domain
workflows ("skills") this harness knows about — the Python-side half of a
Go/Python identity split this project already accepts elsewhere (tool_tiers.go
mirrors tools.py's TOOL_REGISTRY by hand for the same reason: the workflow
layer can't ask the activity layer for this at runtime, and vice versa;
docs/components/tool-registry.md documents that cost as a known, deliberately
accepted one for the native tier). workflows/internal/workflow/skills/registry.go
mirrors this same list (name/description/input_schema) for the gateway's own
GET /skills — that's a second, separate hand-sync point, not this one.

Each entry here must have a matching Go workflow function in
workflows/internal/workflow/skills/ and a
`w.RegisterWorkflowWithOptions(skillswf.<WorkflowType>, workflow.RegisterOptions{Name: "<name>"})`
line in workflows/cmd/loop-worker/main.go, registered under this entry's exact
`name` string. 2026-09-27: `name` IS the Temporal workflow type now — there
used to be a second, separately hand-typed `workflow_type` field here
(PascalCase, not even consistently suffixed across entries), which could
silently drift from `name` with no error until the skill failed to dispatch.
Collapsing them to one string closes that gap; ExecuteChildWorkflow dispatches
by this name directly (turn.go), so there is still no separate Go
name-to-function table to keep in sync — just one string instead of two.

Hand-authored only, never auto-populated or learned from a transcript
(docs/components/turn-pipeline.md, "Skill recording": "There is none, and
there won't be.").
"""

from __future__ import annotations

SKILLS: list[dict] = [
    {
        "name": "journaling",
        "description": (
            "Record a journal entry the user has decided to keep, into today's page in "
            "their Notion journal. Only call this once the user has confirmed something is "
            "actually journal material (not just thinking out loud) — this skill itself "
            "runs its own reasoning turn to find or create the right Notion database, "
            "confirm the entry with the user, and write it."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "entry_text": {"type": "string", "description": "The journal entry's content."},
            },
            "required": ["entry_text"],
        },
    },
    {
        "name": "service_monitoring",
        "description": (
            "Configure durable monitoring for a Kubernetes service. This skill uses Grafana as the "
            "mandatory source of monitoring evidence: it finds the relevant Grafana capability, inspects "
            "the service's actual metrics, dashboards, and existing alerts, selects an appropriate health "
            "signal, asks before making consequential external changes, and arms a monitoring intention. "
            "Use this when the user asks to watch, monitor, or be alerted about a Kubernetes service."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "service": {"type": "string", "description": "Kubernetes service or workload to monitor."},
                "namespace": {"type": "string", "description": "Optional Kubernetes namespace containing the service."},
                "cluster": {"type": "string", "description": "Optional cluster name or identifier."},
                "notify_when": {
                    "type": "string",
                    "description": "What should trigger notification; defaults to the service being unavailable.",
                },
            },
            "required": ["service"],
        },
    },
]
