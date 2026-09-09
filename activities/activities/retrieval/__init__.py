"""`turn_retrieval` staging helpers.

The pre-LLM retrieval fan-out (RoutingWorkflow + memory/tools/skills discovery)
was removed in the turn-pipeline redesign (docs/components/turn-pipeline.md,
Phase 8), and the skill subsystem was removed entirely shortly after. All that
remains here is `staging.py`, the shared `turn_retrieval` read/write used by
`discover_tools` to make a mid-turn tool discovery callable by name on the
turn's next step.
"""
