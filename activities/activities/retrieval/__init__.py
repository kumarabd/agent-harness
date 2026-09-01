"""Request-pipeline retrieval phase — steps 4/5/6/7
(docs/components/request-pipeline/).

Orchestrated by RoutingWorkflow (workflows/internal/workflow/routing.go): a
plan-gated parallel fan-out of the three discovery subsystems, then
ComposeSkill if skill candidates were found. Each activity stages its
results to the `turn_retrieval` table (staging.py) and returns only a
SubsystemResult (status + count) to the workflow.

  - memory.py  — agent-brain memory_search (step 4) — real
  - tools.py   — search_tools / mcp-hub + shell-hub (step 7) — real
  - skills.py  — flat-cosine retrieval over the skill store (step 5) — real,
                 phase 1; design + store in activities/activities/skills/
  - compose.py — merge the retrieved procedures into one (step 6) — real, phase 1
"""

from __future__ import annotations

from .compose import ComposeSkillActivity
from .memory import MemoryRetrieveActivity
from .skills import SkillDiscoverActivity
from .tools import ToolDiscoverActivity

__all__ = [
    "MemoryRetrieveActivity",
    "ToolDiscoverActivity",
    "SkillDiscoverActivity",
    "ComposeSkillActivity",
]
