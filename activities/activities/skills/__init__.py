"""skills — docs/05-architecture-domain-control-loops.md.

Each domain's own system prompt and switch_mode description live in their
own module here, self-registered into base.py's generic registry at import
time — llm.py/model_call.py never name a specific domain; they only read
this package's own generic accessors (names/description_of/descriptions/
prompt_for). Adding a new mode means a new module here (plus its own
registered Go workflow, workflows/internal/workflow/mode_<name>.go) — never
touching llm.py's or model_call.py's own generic code, mirroring the Go
side's own one-file-per-domain shape exactly.
"""

from __future__ import annotations

from . import journaling, service_monitoring  # noqa: F401 — triggers self-registration
from .base import description_of, descriptions, names, prompt_for

__all__ = ["description_of", "descriptions", "names", "prompt_for"]
