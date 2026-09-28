"""skills.base — the generic registry every domain module in this package
self-registers into at import time (register(SkillMode(...))).

Deliberately tiny and domain-agnostic: no code here, and no code in
llm.py/model_call.py, ever names "journaling" or "service_monitoring"
directly — only this package's own per-domain submodules (journaling.py,
service_monitoring.py) do. 2026-09-28: split out of llm.py, which used to
define MODE_TURNS/JOURNALING_SYSTEM_PROMPT/SERVICE_MONITORING_SYSTEM_PROMPT
directly — the same class of "domain-specific content in a generic file"
violation that "no domain-specific skill items in tenant_worker.py" had
already ruled out elsewhere; this closes the same gap in llm.py/
model_call.py. Mirrors the Go side's own one-file-per-domain shape
(workflows/internal/workflow/mode_journaling.go, mode_service_monitoring.go).

No dependency on llm.py, capabilities.py, or prompt.py — this package is a
leaf. llm.py/model_call.py import it, never the reverse, so it introduces
no import-cycle risk at all (unlike the MODE_TURNS-load-order concern this
replaces, which no longer applies now that capabilities.py holds no
per-tenant skill state to rebuild).
"""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True)
class SkillMode:
    name: str
    # switch_mode's own per-option description (llm.py's _switch_mode_schema
    # reads this generically via description_of/descriptions below).
    description: str
    system_prompt: str


_REGISTRY: dict[str, SkillMode] = {}


def register(mode: SkillMode) -> None:
    _REGISTRY[mode.name] = mode


def names() -> list[str]:
    """The complete set of names genuinely dispatchable as a TurnInput-shaped
    mode-turn workflow in Go (cmd/loop-worker/main.go) — a fixed,
    deployment-wide fact (which domain modules exist in this package),
    identical for every tenant. Per-tenant *enablement* of a name is a
    separate concern (llm.ENABLED_MODES), not this package's job."""
    return list(_REGISTRY)


def description_of(name: str) -> str:
    return _REGISTRY[name].description


def descriptions() -> dict[str, str]:
    return {name: mode.description for name, mode in _REGISTRY.items()}


def prompt_for(name: str) -> str | None:
    mode = _REGISTRY.get(name)
    return mode.system_prompt if mode else None
