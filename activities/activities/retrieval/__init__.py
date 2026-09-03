"""Request-pipeline retrieval phase — steps 4/5/7 (docs/components/request-pipeline/).

Orchestrated by RoutingWorkflow (workflows/internal/workflow/routing.go): a
plan-gated parallel fan-out of the discovery subsystems, each staging its
results to `turn_retrieval` (staging.py) and returning a SubsystemResult.

  - memory.py — agent-brain memory_search (step 4), per turn
  - tools.py  — search_tools / mcp-hub + shell-hub (step 7), per turn
  - skills.py — flat-cosine retrieval over the skill store (step 5), per task-run

ComposeSkill (the old step 6) is removed — Phase 3C, the planning turn drafts
the plan from SkillDiscover's rows.
"""

from __future__ import annotations

from .memory import MemoryRetrieveActivity
from .skills import SkillDiscoverActivity
from .tools import ToolDiscoverActivity

__all__ = [
    "MemoryRetrieveActivity",
    "ToolDiscoverActivity",
    "SkillDiscoverActivity",
]
