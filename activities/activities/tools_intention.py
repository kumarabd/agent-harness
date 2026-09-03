"""Intention tools — docs/components/proactivity.md, "The agent's tools".

Six handlers the agent calls from inside any turn to garden its own standing
intentions. Each is a thin wrapper over the Temporal client (`ctx.temporal_client`,
threaded in by ToolCallActivity): an intention *is* an `IntentionWorkflow`
execution, so create = start_workflow, revise/snooze = signal, cancel = cancel,
list/inspect = list_workflows + the `status` query. There is no intentions table.

Intentions are scoped to the **user-stable scope** of the creating session
(`ids.user_scope_of` — session_key with any per-branch `:session:`/`:thread:`
suffix stripped): workflow id `intn:<scope>:<slug>`. For web that scope is the
user; for a shared Discord channel it's the channel (the best available — the
harness has no user primitive). A fired intention wakes that scope's canonical
session coordinator (SignalWithStart recreates it if it has idled out).

`list_intentions` scopes server-side on the **`IntentionUser` Search Attribute**
(set by `IntentionWorkflow`, alongside `IntentionKind` / `IntentionState`) —
`ListWorkflowExecutions` filtered on it, not "list everything and match the
workflow-id prefix". The three attributes are registered on the namespace as a
deploy step; the workflow's upsert of an unregistered attribute is a harmless
no-op, but a `list_intentions` query against one is not — it fails, and that
surfaces (no fallback).

No import of `.tools` here (would be circular — tools.py registers these) — the
`ctx` argument is duck-typed; `from __future__ import annotations` keeps the
type hints as strings.
"""

from __future__ import annotations

import logging
import os
import re
from datetime import datetime, timedelta, timezone
from typing import TYPE_CHECKING

from temporalio.client import (
    Schedule,
    ScheduleActionStartWorkflow,
    ScheduleIntervalSpec,
    ScheduleSpec,
)

from .ids import user_scope_of

if TYPE_CHECKING:
    from .tools import ToolContext

logger = logging.getLogger(__name__)

_INTENTION_WORKFLOW = "IntentionWorkflow"
_TASK_QUEUE = os.environ.get("TEMPORAL_TASK_QUEUE", "agent-loop")
_KINDS = {"time", "deadline", "condition", "state", "event", "inactivity", "schedule"}

# A recurring intention is a Temporal Schedule (id prefix "intn-sched:") whose
# action starts a one-shot IntentionWorkflow (id prefix "intn:") per tick.
_SCHED_PREFIX = "intn-sched:"
_WF_PREFIX = "intn:"


def _slug(objective: str) -> str:
    words = re.findall(r"[a-z0-9]+", objective.lower())
    return "-".join(words[:6])[:60] or "intention"


def _rfc3339(value: str, field: str) -> str:
    """Normalise a model-supplied timestamp to RFC3339 (what Go's time.Time
    JSON unmarshal expects)."""
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError(f"{field} is not a valid ISO-8601 timestamp: {value!r}") from exc
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _client(ctx: "ToolContext"):
    if getattr(ctx, "temporal_client", None) is None:
        raise RuntimeError("intention tools require a Temporal client (not wired into this ToolCallActivity)")
    return ctx.temporal_client


def _scope(ctx: "ToolContext") -> str:
    """The user-stable intention namespace for this turn — see ids.user_scope_of.
    Also the session_key a fired intention wakes."""
    return user_scope_of(ctx.session_key)


def _own_id(ctx: "ToolContext", intention_id: str) -> str:
    scope = _scope(ctx)
    if str(intention_id).startswith(f"{_SCHED_PREFIX}{scope}:") or str(intention_id).startswith(f"{_WF_PREFIX}{scope}:"):
        return intention_id
    raise ValueError(f"intention {intention_id!r} does not belong to you")


async def _create_recurring(arguments: dict, ctx: "ToolContext", scope: str, objective: str, why: str) -> dict:
    cron = (arguments.get("cron") or "").strip()
    every = arguments.get("every_seconds")
    if not cron and not every:
        raise ValueError("kind=schedule requires 'cron' (UTC) or 'every_seconds'")
    slug = _slug(objective)
    sched_id = f"{_SCHED_PREFIX}{scope}:{slug}"
    wf_id = f"{_WF_PREFIX}{scope}:{slug}"

    spec = ScheduleSpec(
        cron_expressions=[cron] if cron else [],
        intervals=[ScheduleIntervalSpec(every=timedelta(seconds=float(every)))] if every else [],
    )
    action = ScheduleActionStartWorkflow(
        _INTENTION_WORKFLOW,
        # kind="time" with no fire_at ⇒ the workflow fires immediately on start,
        # i.e. once per schedule tick (see IntentionWorkflow's time case).
        {"intention_id": wf_id, "session_key": scope, "objective": objective, "why": why, "kind": "time"},
        id=wf_id,
        task_queue=_TASK_QUEUE,
    )
    try:
        await _client(ctx).create_schedule(sched_id, Schedule(action=action, spec=spec))
    except Exception as exc:  # noqa: BLE001
        if "already" in str(exc).lower():
            return {"intention_id": sched_id, "note": "a recurring intention with this objective already exists — cancel it first"}
        raise
    logger.info("create_intention: armed recurring %s (%s)", sched_id, cron or f"every {every}s")
    return {"intention_id": sched_id, "armed": True, "recurring": True}


async def create_intention(arguments: dict, ctx: "ToolContext") -> dict:
    objective = (arguments.get("objective") or "").strip()
    kind = (arguments.get("kind") or "").strip()
    if not objective:
        raise ValueError("create_intention requires a non-empty 'objective'")
    if kind not in _KINDS:
        raise ValueError(f"'kind' must be one of {sorted(_KINDS)}")

    scope = _scope(ctx)
    why = (arguments.get("why") or "").strip()

    if kind == "schedule":
        return await _create_recurring(arguments, ctx, scope, objective, why)

    wf_input: dict = {
        "intention_id": "",
        "session_key": scope,  # the canonical session a fire wakes (ids.user_scope_of)
        "objective": objective,
        "why": why,
        "kind": kind,
    }

    if kind in ("time", "deadline"):
        if not arguments.get("fire_at"):
            raise ValueError(f"kind={kind} requires 'fire_at' (ISO-8601)")
        wf_input["fire_at"] = _rfc3339(arguments["fire_at"], "fire_at")
    elif kind in ("condition", "state", "event"):
        probe = arguments.get("probe") or {}
        if not probe.get("tool") or not probe.get("predicate"):
            raise ValueError(f"kind={kind} requires 'probe' with 'tool' and 'predicate'")
        wf_input["probe"] = {
            "tool": probe["tool"],
            "args": probe.get("args") or {},
            "predicate": probe["predicate"],
        }
        if arguments.get("poll_every_seconds"):
            wf_input["poll_every"] = int(float(arguments["poll_every_seconds"]) * 1_000_000_000)
        if arguments.get("expires_at"):
            wf_input["expires_at"] = _rfc3339(arguments["expires_at"], "expires_at")
    elif kind == "inactivity":
        if not arguments.get("idle_for_seconds"):
            raise ValueError("kind=inactivity requires 'idle_for_seconds'")
        wf_input["idle_for"] = int(float(arguments["idle_for_seconds"]) * 1_000_000_000)

    intention_id = f"intn:{scope}:{_slug(objective)}"
    wf_input["intention_id"] = intention_id

    try:
        await _client(ctx).start_workflow(
            _INTENTION_WORKFLOW, wf_input, id=intention_id, task_queue=_TASK_QUEUE
        )
    except Exception as exc:  # noqa: BLE001 — AlreadyStarted's class name varies across SDK versions
        msg = str(exc).lower()
        if "already" in msg and ("start" in msg or "exist" in msg or "running" in msg):
            return {
                "intention_id": intention_id,
                "note": "an intention with a matching objective is already armed — revise or cancel it instead",
            }
        raise
    logger.info("create_intention: armed %s (kind=%s)", intention_id, kind)
    return {"intention_id": intention_id, "armed": True}


def _q_lit(value: str) -> str:
    """Single-quote a value for a Temporal visibility query (escape embedded ')."""
    return "'" + value.replace("'", "''") + "'"


async def list_intentions(arguments: dict, ctx: "ToolContext") -> dict:
    scope = _scope(ctx)
    sched_prefix = f"{_SCHED_PREFIX}{scope}:"
    client = _client(ctx)
    out: list[dict] = []

    # Server-side scope filter on the IntentionUser Search Attribute — no
    # client-side workflow-id matching. The per-workflow `status` query still
    # gives the rich fields (objective, fire count) that aren't attributes.
    # No fallback: if the query fails (attributes not registered on the
    # namespace), that surfaces rather than silently returning a partial list.
    query = (
        "WorkflowType = 'IntentionWorkflow' "
        f"AND IntentionUser = {_q_lit(scope)} "
        "AND ExecutionStatus = 'Running'"
    )
    async for wf in client.list_workflows(query):
        try:
            out.append(await client.get_workflow_handle(wf.id).query("status"))
        except Exception:  # noqa: BLE001 — a just-closed / mid-continue-as-new workflow
            out.append({"intention_id": wf.id, "state": "unknown"})

    # Schedules aren't workflow executions until they fire, so they carry no
    # Search Attributes — scope them by id prefix as before.
    async for sched in client.list_schedules():
        if not sched.id.startswith(sched_prefix):
            continue
        out.append(await _describe_schedule(client, sched.id))

    return {"intentions": out}


async def _describe_schedule(client, sched_id: str) -> dict:
    try:
        desc = await client.get_schedule_handle(sched_id).describe()
        spec = desc.schedule.spec
        cadence = (spec.cron_expressions or [None])[0] or (
            f"every {spec.intervals[0].every.total_seconds():g}s" if spec.intervals else "?"
        )
        return {
            "intention_id": sched_id,
            "kind": "schedule",
            "state": "paused" if desc.schedule.state.paused else "armed",
            "cadence": cadence,
            "objective": (desc.schedule.action.args[0] or {}).get("objective", ""),
        }
    except Exception as exc:  # noqa: BLE001
        return {"intention_id": sched_id, "kind": "schedule", "state": "unknown", "note": str(exc)}


async def inspect_intention(arguments: dict, ctx: "ToolContext") -> dict:
    intention_id = _own_id(ctx, arguments["intention_id"])
    client = _client(ctx)
    if intention_id.startswith(_SCHED_PREFIX):
        return await _describe_schedule(client, intention_id)
    try:
        return await client.get_workflow_handle(intention_id).query("status")
    except Exception as exc:  # noqa: BLE001
        return {"intention_id": intention_id, "state": "not-found", "note": str(exc)}


async def revise_intention(arguments: dict, ctx: "ToolContext") -> dict:
    intention_id = _own_id(ctx, arguments["intention_id"])
    if intention_id.startswith(_SCHED_PREFIX):
        return {
            "intention_id": intention_id,
            "revised": False,
            "note": "recurring intentions can't be revised in place — cancel it and create a new one",
        }
    revise: dict = {}
    if arguments.get("objective"):
        revise["objective"] = arguments["objective"].strip()
    if arguments.get("why"):
        revise["why"] = arguments["why"].strip()
    if arguments.get("fire_at"):
        revise["fire_at"] = _rfc3339(arguments["fire_at"], "fire_at")
    if arguments.get("poll_every_seconds"):
        revise["poll_every"] = int(float(arguments["poll_every_seconds"]) * 1_000_000_000)
    if not revise:
        raise ValueError("revise_intention needs at least one of objective / why / fire_at / poll_every_seconds")
    await _client(ctx).get_workflow_handle(intention_id).signal("revise", revise)
    return {"intention_id": intention_id, "revised": True}


async def snooze_intention(arguments: dict, ctx: "ToolContext") -> dict:
    intention_id = _own_id(ctx, arguments["intention_id"])
    if intention_id.startswith(_SCHED_PREFIX):
        return {
            "intention_id": intention_id,
            "snoozed": False,
            "note": "snooze doesn't apply to a recurring intention — cancel it if you want it to stop",
        }
    by = arguments.get("by_seconds")
    if not by or float(by) <= 0:
        raise ValueError("snooze_intention requires a positive 'by_seconds'")
    await _client(ctx).get_workflow_handle(intention_id).signal("snooze", int(float(by) * 1_000_000_000))
    return {"intention_id": intention_id, "snoozed_by_seconds": float(by)}


async def cancel_intention(arguments: dict, ctx: "ToolContext") -> dict:
    intention_id = _own_id(ctx, arguments["intention_id"])
    client = _client(ctx)
    try:
        if intention_id.startswith(_SCHED_PREFIX):
            await client.get_schedule_handle(intention_id).delete()
        else:
            await client.get_workflow_handle(intention_id).cancel()
    except Exception as exc:  # noqa: BLE001
        return {"intention_id": intention_id, "cancelled": False, "note": str(exc)}
    return {"intention_id": intention_id, "cancelled": True}
