"""Wake tools — docs/components/proactivity.md, "The agent's tools".

Two model-facing tools, following the registry's own resolved pattern
(tool-registry.md, "Resolved: Three-Layer Tool Taxonomy" — CRUD on one construct,
not N distinct intents): `arm_wake` (its own schema — the one genuinely complex
operation) and `manage_wake(action in {list, inspect, revise, cancel})`, a thin
dispatcher over the functions below.

**A wake is not a construct the harness stores.** A time wake is a Temporal
*Schedule*, and the Schedule IS the record: listed, paused, retimed and cancelled
through Temporal's own schedule APIs, with calendar maths, catch-up and overlap
policy already solved. There is no wakes table, which is what made the old
`intn:<scope>:<slug>` identity problem disappear — the agent supplies `name`, so a
duplicate is a Temporal conflict returned to the model, not a slug guessed from
free text.

**Why the tool is called `arm_wake` and not `create_schedule`:** external tool
names arrive dynamically through `discover_tools`, and mcp-hub's disambiguation
covers collision within its own tier only — so a native tool colliding with a
discovered Calendar/Reminders tool has no resolution path. `schedule`, `reminder`,
`notification`, `event`, `timer` are exactly the words such a server claims.
`Wake` is already this codebase's own vocabulary for waking a session
(`WakeSignalName`, `WakeSessionActivity`); `arm` is nobody's.

**Two kinds of wake, one verb.** A time wake is a Temporal Schedule
(`wake:<scope>:<name>`); an event wake is a row in mcp-hub
(`watch:<scope>:<name>`) that pairs an engine's `event_key` with this session. Both
are addressed, listed and cancelled through `manage_wake` — the prefix says which
store the wake lives in, so nothing has to guess. This is the one place two sources
get unioned, and it is deliberate: to the agent a wake is a wake, however it is
triggered.

**What is deliberately NOT here:** any way to make an external system notify the
*user* at a time. The agent already has those tools through mcp-hub connections
and should use them — the harness is only needed to wake the agent's own
reasoning.
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
    ScheduleOverlapPolicy,
    SchedulePolicy,
    ScheduleSpec,
    ScheduleState,
    ScheduleUpdate,
)

from . import mcp_hub
from .ids import user_scope_of

if TYPE_CHECKING:
    from .tools import ToolContext

logger = logging.getLogger(__name__)

_WAKE_WORKFLOW = "WakeWorkflow"
# Same note as every other task-queue reference here: this is the shared, tenant-agnostic
# workflow-task queue every tenant's loop-worker pool polls identically.
_TASK_QUEUE = os.environ.get("TEMPORAL_WORKFLOW_TASK_QUEUE", "agent-loop")
_TENANT_SLUG = os.environ.get("TENANT_SLUG", "")

# Every wake Schedule is named with this prefix, scoped by the user-stable scope,
# so `list` is an id-prefix filter over Temporal's own schedule list — no Search
# Attributes (and so no namespace deploy step) are involved.
_SCHED_PREFIX = "wake:"

# Event wakes live in mcp-hub's subscriptions table, not in Temporal, so they get
# their own prefix: `_own_id` can then check both without a lookup, and
# inspect/cancel dispatch on the prefix instead of asking each store in turn.
_WATCH_PREFIX = "watch:"

_NAME_RE = re.compile(r"^[a-z0-9][a-z0-9-]{0,59}$")


def _client(ctx: "ToolContext"):
    if getattr(ctx, "temporal_client", None) is None:
        raise RuntimeError("wake tools require a Temporal client (not wired into this ToolCallActivity)")
    return ctx.temporal_client


def _scope(ctx: "ToolContext") -> str:
    """The user-stable wake namespace for this turn — see ids.user_scope_of. Also
    the session_key a fired wake wakes."""
    return user_scope_of(ctx.session_key)


def _name(arguments: dict) -> str:
    """The wake's name, supplied by the agent and required. This is the identity —
    never derived from the objective's free text, so two arms of the same request
    collide on purpose instead of by accident."""
    name = (arguments.get("name") or "").strip().lower()
    if not name:
        raise ValueError("arm_wake requires a short 'name' (lowercase, dashes) that identifies this wake")
    if not _NAME_RE.match(name):
        raise ValueError(f"wake name {name!r} must be lowercase letters, digits and dashes, 1-60 chars")
    return name


def _own_id(ctx: "ToolContext", wake_id: str) -> str:
    scope = _scope(ctx)
    for prefix in (_SCHED_PREFIX, _WATCH_PREFIX):
        if str(wake_id).startswith(f"{prefix}{scope}:"):
            return wake_id
    raise ValueError(f"wake {wake_id!r} does not belong to you")


def _rfc3339(value: str, field: str) -> str:
    """Normalise a model-supplied timestamp to RFC3339 (what Go's time.Time JSON
    unmarshal expects)."""
    try:
        dt = datetime.fromisoformat(value.replace("Z", "+00:00"))
    except ValueError as exc:
        raise ValueError(f"{field} is not a valid ISO-8601 timestamp: {value!r}") from exc
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.astimezone(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")


def _trigger_spec(arguments: dict) -> tuple[ScheduleSpec, str, bool]:
    """The schedule's cadence, its human description, and whether it fires once.

    One-shots use `remaining_actions=1` on the Schedule rather than a separate
    timer workflow, so time wakes have exactly one mechanism and one listing path.
    """
    at = (arguments.get("at") or "").strip()
    cron = (arguments.get("cron") or "").strip()
    every = arguments.get("every_seconds")

    if at:
        fire_at = datetime.fromisoformat(_rfc3339(at, "at").replace("Z", "+00:00"))
        delta = fire_at - datetime.now(timezone.utc)
        if delta.total_seconds() <= 0:
            raise ValueError("'at' is in the past — give a future time or use 'every_seconds'")
        return (
            ScheduleSpec(
                start_at=fire_at,
                intervals=[ScheduleIntervalSpec(every=timedelta(days=3650))],
                end_at=fire_at + timedelta(seconds=1),
            ),
            f"once at {_rfc3339(at, 'at')}",
            True,
        )
    if cron:
        return ScheduleSpec(cron_expressions=[cron]), f"cron {cron} (UTC)", False
    if every:
        seconds = float(every)
        if seconds <= 0:
            raise ValueError("'every_seconds' must be positive")
        return ScheduleSpec(intervals=[ScheduleIntervalSpec(every=timedelta(seconds=seconds))]), f"every {seconds:g}s", False
    raise ValueError("arm_wake needs one of: 'at' (ISO-8601), 'cron' (UTC), or 'every_seconds'")


async def _arm_event_wake(ctx: "ToolContext", name: str, objective: str, on_event) -> dict:
    """Arm a watch: pair one of an engine's events with this session.

    The objective is the agent's, exactly as for a time wake, and it is the *more*
    important of the two halves: an event often only says that something happened,
    and what to do about it is the armer's standing instruction — sent now because
    the engine raising the notice cannot possibly know it. The notice supplies the
    other half, what actually happened, and the two reach the same turn together.
    """
    event_key = (on_event.get("event_key") if isinstance(on_event, dict) else str(on_event)) or ""
    event_key = event_key.strip()
    if not event_key:
        raise ValueError(
            "on_event needs an 'event_key' — the thing to watch, exactly as the engine that "
            "raises it names it"
        )

    scope = _scope(ctx)
    wake_id = f"{_WATCH_PREFIX}{scope}:{name}"
    try:
        await mcp_hub.arm_subscription(
            wake_id=wake_id, event_key=event_key, invoke_id=scope, objective=objective
        )
    except (mcp_hub.McpHubNotConfiguredError, mcp_hub.McpHubCallError) as exc:
        # Surfaced, never swallowed: an arm the agent believes succeeded but which
        # registered nothing is an alert that silently never arrives.
        raise ValueError(f"could not arm the watch: {exc}") from exc

    logger.info("arm_wake: watching %s as %s", event_key, wake_id)
    return {"wake_id": wake_id, "armed": True, "on_event": event_key, "objective": objective}


async def arm_wake(arguments: dict, ctx: "ToolContext") -> dict:
    name = _name(arguments)
    objective = (arguments.get("objective") or "").strip()
    if not objective:
        raise ValueError("arm_wake requires a non-empty 'objective' — what you should do when you wake")
    why = (arguments.get("why") or "").strip()

    if arguments.get("on_event") is not None:
        return await _arm_event_wake(ctx, name, objective, arguments["on_event"])

    scope = _scope(ctx)
    spec, cadence, one_shot = _trigger_spec(arguments)

    wake_id = f"{_SCHED_PREFIX}{scope}:{name}"
    action = ScheduleActionStartWorkflow(
        _WAKE_WORKFLOW,
        {
            "wake_id": wake_id,
            "session_key": scope,  # the canonical session a wake wakes
            "tenant_slug": _TENANT_SLUG,
            "objective": objective,
            "why": why,
        },
        id=wake_id,
        task_queue=_TASK_QUEUE,
    )
    schedule = Schedule(
        action=action,
        spec=spec,
        # SKIP, not ALLOW_ALL: two wakes stacked on one session is never wanted —
        # the coordinator would just fold the second into the first.
        policy=SchedulePolicy(overlap=ScheduleOverlapPolicy.SKIP),
    )
    if one_shot:
        # A one-shot is a Schedule that runs once, so time wakes have exactly one
        # mechanism and one listing path — no separate timer workflow, and no
        # second way to enumerate them.
        schedule.state.remaining_actions = 1

    try:
        await _client(ctx).create_schedule(wake_id, schedule)
    except Exception as exc:  # noqa: BLE001 — the SDK's already-exists error class varies
        if "already" in str(exc).lower() or "exists" in str(exc).lower():
            # Not an error the model should paper over: the identity is the name it
            # chose, so this is a real duplicate it must resolve explicitly.
            return {
                "wake_id": wake_id,
                "armed": False,
                "note": (
                    f"a wake named {name!r} already exists for you. Revise it with "
                    "manage_wake(action='revise'), or cancel it and arm a new one. Do not arm a second "
                    "wake for the same thing."
                ),
            }
        raise

    logger.info("arm_wake: armed %s (%s)", wake_id, cadence)
    return {"wake_id": wake_id, "armed": True, "cadence": cadence, "one_shot": one_shot}


async def _describe(client, wake_id: str) -> dict:
    try:
        desc = await client.get_schedule_handle(wake_id).describe()
        spec = desc.schedule.spec
        cadence = (spec.cron_expressions or [None])[0] or (
            f"every {spec.intervals[0].every.total_seconds():g}s" if spec.intervals else "?"
        )
        info = desc.info
        return {
            "wake_id": wake_id,
            "state": "paused" if desc.schedule.state.paused else "armed",
            "cadence": cadence,
            "one_shot": desc.schedule.state.remaining_actions == 1,
            "objective": (desc.schedule.action.args[0] or {}).get("objective", ""),
            "why": (desc.schedule.action.args[0] or {}).get("why", ""),
            "next_fire": info.next_action_times[0].isoformat() if info.next_action_times else "",
            "last_fire": info.recent_actions[0].scheduled_at.isoformat() if info.recent_actions else "",
            "fires": info.num_actions,
        }
    except Exception as exc:  # noqa: BLE001
        return {"wake_id": wake_id, "state": "unknown", "note": str(exc)}


async def list_wakes(arguments: dict, ctx: "ToolContext") -> dict:
    scope = _scope(ctx)
    client = _client(ctx)
    out: list[dict] = []
    # list_schedules is `async def` in temporalio — it must be awaited first to get
    # the ScheduleAsyncIterator, then iterated; `async for` directly on the
    # coroutine is a real bug, verified against the installed SDK (1.32.0).
    async for sched in await client.list_schedules():
        if sched.id.startswith(f"{_SCHED_PREFIX}{scope}:"):
            out.append(await _describe(client, sched.id))

    # Event watches live in mcp-hub. This union is the one place the two stores meet,
    # and it is deliberate: to the agent a wake is a wake, however it is triggered.
    note = ""
    try:
        subscriptions = await mcp_hub.list_subscriptions()
    except mcp_hub.McpHubNotConfiguredError:
        # No hub configured means no event watch can exist for this tenant either, so
        # the schedule list above is the whole truth rather than a silent gap.
        subscriptions = []
    except mcp_hub.McpHubCallError as exc:
        # A hub that is down must not hide the time wakes above — but it must not read
        # as "you have no watches" either, or the agent arms a duplicate of one it
        # already has. So the list is returned, marked, rather than failed or faked.
        subscriptions = []
        note = f"event watches could not be read, so this list may be incomplete: {exc}"
    for subscription in subscriptions:
        wake_id = subscription.get("wake_id") or ""
        if wake_id.startswith(f"{_WATCH_PREFIX}{scope}:"):
            out.append({
                "wake_id": wake_id,
                "state": "armed",
                "cadence": f"when {subscription['event_key']} fires",
                "on_event": subscription["event_key"],
                "objective": subscription.get("objective", ""),
            })
    return {"wakes": out, "note": note} if note else {"wakes": out}


async def inspect_wake(arguments: dict, ctx: "ToolContext") -> dict:
    wake_id = _own_id(ctx, arguments["wake_id"])
    if wake_id.startswith(_WATCH_PREFIX):
        try:
            subscriptions = await mcp_hub.list_subscriptions()
        except (mcp_hub.McpHubNotConfiguredError, mcp_hub.McpHubCallError) as exc:
            return {"wake_id": wake_id, "state": "unknown", "note": str(exc)}
        for subscription in subscriptions:
            if subscription.get("wake_id") == wake_id:
                return {
                    "wake_id": wake_id,
                    "state": "armed",
                    "cadence": f"when {subscription['event_key']} fires",
                    "on_event": subscription["event_key"],
                    "objective": subscription.get("objective", ""),
                    # Empty for a watch on purpose: the engine's notice fills the
                    # why slot when it fires, so there is no armer-authored one.
                    "why": "",
                }
        return {"wake_id": wake_id, "state": "unknown", "note": "no such event watch"}
    return await _describe(_client(ctx), wake_id)


async def revise_wake(arguments: dict, ctx: "ToolContext") -> dict:
    wake_id = _own_id(ctx, arguments["wake_id"])
    if wake_id.startswith(_WATCH_PREFIX):
        # A watch revises the two things it has: what it should do (objective) and
        # what it is listening for (event_key). Everything else a time wake can
        # change — cadence, pause — a watch does not have, and the guard below names
        # those rather than accepting one and ignoring it.
        on_event = arguments.get("on_event")
        event_key = (on_event or {}).get("event_key") if isinstance(on_event, dict) else None
        objective = arguments.get("objective")
        if event_key is None and objective is None:
            raise ValueError(
                "revising a watch needs a new 'objective' or a new 'on_event'. It has no cadence, "
                "and 'why' is filled by the engine's notice when it fires."
            )
        try:
            revised = await mcp_hub.revise_subscription(
                wake_id,
                event_key=event_key.strip() if isinstance(event_key, str) else None,
                objective=objective.strip() if isinstance(objective, str) else None,
            )
        except (mcp_hub.McpHubNotConfiguredError, mcp_hub.McpHubCallError) as exc:
            return {"wake_id": wake_id, "revised": False, "note": str(exc)}
        return {"wake_id": wake_id, "revised": revised}
    if not any(k in arguments for k in ("at", "cron", "every_seconds", "objective", "why", "paused")):
        raise ValueError("revise_wake needs at least one of at / cron / every_seconds / objective / why / paused")
    handle = _client(ctx).get_schedule_handle(wake_id)
    try:
        current = (await handle.describe()).schedule
    except Exception as exc:  # noqa: BLE001
        return {"wake_id": wake_id, "revised": False, "note": str(exc)}

    # Temporal's update replaces the whole Schedule, so the new one is built from
    # the current one: a revise must not silently clear what it didn't mention.
    spec = current.spec
    one_shot = False
    if any(k in arguments for k in ("at", "cron", "every_seconds")):
        spec, _, one_shot = _trigger_spec(arguments)

    args = dict((current.action.args or [{}])[0] or {})
    if arguments.get("objective"):
        args["objective"] = arguments["objective"].strip()
    if arguments.get("why") is not None:
        args["why"] = (arguments.get("why") or "").strip()

    state = ScheduleState(
        paused=bool(arguments["paused"]) if arguments.get("paused") is not None else current.state.paused,
        remaining_actions=1 if one_shot else current.state.remaining_actions,
    )

    action = ScheduleActionStartWorkflow(
        current.action.workflow, args, id=current.action.id, task_queue=current.action.task_queue
    )
    try:
        await handle.update(
            ScheduleUpdate(schedule=Schedule(action=action, spec=spec, policy=current.policy, state=state))
        )
    except Exception as exc:  # noqa: BLE001
        return {"wake_id": wake_id, "revised": False, "note": str(exc)}
    return {"wake_id": wake_id, "revised": True}


async def cancel_wake(arguments: dict, ctx: "ToolContext") -> dict:
    wake_id = _own_id(ctx, arguments["wake_id"])
    if wake_id.startswith(_WATCH_PREFIX):
        try:
            cancelled = await mcp_hub.cancel_subscription(wake_id)
        except (mcp_hub.McpHubNotConfiguredError, mcp_hub.McpHubCallError) as exc:
            return {"wake_id": wake_id, "cancelled": False, "note": str(exc)}
        # False here means nothing was armed under that id — a real answer, not a
        # failure, and one the agent should not be told was a success.
        return {"wake_id": wake_id, "cancelled": cancelled}
    try:
        await _client(ctx).get_schedule_handle(wake_id).delete()
    except Exception as exc:  # noqa: BLE001
        return {"wake_id": wake_id, "cancelled": False, "note": str(exc)}
    return {"wake_id": wake_id, "cancelled": True}


_MANAGE_ACTIONS = {
    "list": list_wakes,
    "inspect": inspect_wake,
    "revise": revise_wake,
    "cancel": cancel_wake,
}


async def manage_wake(arguments: dict, ctx: "ToolContext") -> dict:
    """The one model-facing tool for every op except arm — dispatches on
    `arguments["action"]`. No fallback: an unknown/missing action is a real error,
    not a silent no-op."""
    action = arguments.get("action")
    handler = _MANAGE_ACTIONS.get(action)
    if handler is None:
        raise ValueError(f"manage_wake: unknown action {action!r}, expected one of {sorted(_MANAGE_ACTIONS)}")
    return await handler(arguments, ctx)
