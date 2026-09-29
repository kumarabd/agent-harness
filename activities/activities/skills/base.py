"""Skill definitions own domain prompts, allowed tools and completion evidence.
The generic Temporal runtime calls this contract without naming any domain."""

from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any


@dataclass(frozen=True)
class SkillToolContext:
    pool: Any
    temporal_client: Any
    session_key: str
    tool_call_id: str


@dataclass(frozen=True)
class SkillDefinition:
    name: str
    description: str
    system_prompt: str
    # User-facing names recognized by explicit start/stop commands.
    aliases: tuple[str, ...]
    # Exact tools on the external backend; no shell or arbitrary proxy access.
    backend: str
    read_tools: frozenset[str]
    write_tools: frozenset[str]
    native_tools: dict[str, Callable[[dict, SkillToolContext], Awaitable[dict]]]
    validate: Callable[[str, dict, str, list[dict]], None]
    complete: Callable[[str, dict, list[dict]], bool]


_REGISTRY: dict[str, SkillDefinition] = {}


def register(skill: SkillDefinition) -> None:
    if skill.name in _REGISTRY:
        raise ValueError(f"duplicate skill: {skill.name}")
    _REGISTRY[skill.name] = skill


def get(name: str) -> SkillDefinition:
    return _REGISTRY[name]


def names() -> list[str]:
    return list(_REGISTRY)


def description_of(name: str) -> str:
    return get(name).description
