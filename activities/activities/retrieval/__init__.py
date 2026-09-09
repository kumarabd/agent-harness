"""`turn_retrieval` staging helpers.

The pre-LLM retrieval fan-out (RoutingWorkflow + memory/tools/skills discovery)
was removed in the turn-pipeline redesign (docs/components/turn-pipeline.md,
Phase 8) — the model now pulls memory / skills / tools on demand via the
`search_memory` / `load_skill` / `discover_tools` meta-tools. All that remains
here is `staging.py`, the shared `turn_retrieval` read/write used by
`discover_tools` (to make a mid-turn discovery callable by name on the next
step) and by `load_skill` (to record which procedure a run used, for the
RecordSkill EMA loop).
"""
