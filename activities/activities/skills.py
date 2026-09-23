"""skills.py — docs/05-architecture-domain-control-loops.md, docs/components/
turn-pipeline.md ("Skills"). The hand-maintained registry of authored domain
workflows ("skills") this harness knows about — the Python-side half of a
Go/Python identity split this project already accepts elsewhere (tool_tiers.go
mirrors tools.py's TOOL_REGISTRY by hand for the same reason: the workflow
layer can't ask the activity layer for this at runtime, and vice versa;
docs/components/tool-registry.md documents that cost as a known, deliberately
accepted one for the native tier).

Each entry here must have a matching `w.RegisterWorkflow(wf.<WorkflowType>)`
line in workflows/cmd/loop-worker/main.go, keyed by the exact `workflow_type`
string. That's the only Go-side bookkeeping a new skill needs — Temporal's
ExecuteChildWorkflow dispatches by that registered string directly (turn.go),
so there is no separate Go name-to-function table to keep in sync.

Hand-authored only, never auto-populated or learned from a transcript
(docs/components/turn-pipeline.md, "Skill recording": "There is none, and
there won't be.").
"""

from __future__ import annotations

SKILLS: list[dict] = [
    {
        "name": "draft_note",
        "description": (
            "Compose a short note to someone and, after explicit approval, "
            "record it as sent. Use this for a 'draft and send a note/message' "
            "request that should go through a human approval step before it's "
            "final — a harness-validation skill, not a production one."
        ),
        "input_schema": {
            "type": "object",
            "properties": {
                "recipient": {"type": "string", "description": "Who the note is for."},
                "note_text": {"type": "string", "description": "The note's content."},
            },
            "required": ["recipient", "note_text"],
        },
        "workflow_type": "DraftNoteSkillWorkflow",
    },
]
