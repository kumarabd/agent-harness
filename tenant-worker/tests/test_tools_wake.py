"""tools_wake.py — arming, listing, revising and cancelling wakes.

This is the surface that had no test at all when a real bug shipped: `WakeSession`
was missing `id_reuse_policy=ALLOW_DUPLICATE`, so a wake could not restart a
coordinator that had gone idle on its TTL. Nothing in the package could have caught
it, and nothing here reaches Temporal or mcp-hub — both are injected.

The two stores are the thing to keep straight. A time wake is a Temporal Schedule
(`wake:`); an event watch is a row in mcp-hub (`watch:`). Every manage verb has to
land on the right one, which is what most of these tests are checking.
"""

from datetime import datetime, timedelta, timezone

import pytest

from tenant_worker import tools_wake
from tenant_worker.tools import ToolContext

SESSION_KEY = "agent:main:web:user:u1:session:abc"
SCOPE = "agent:main:web:user:u1"


# --------------------------------------------------------------------------- fakes


class _NS:
    """A throwaway object graph — enough shape for the attribute walks in
    _describe, without pulling in the SDK's dataclasses."""

    def __init__(self, **kwargs):
        self.__dict__.update(kwargs)


def make_description(
    *,
    cron=None,
    every_seconds=None,
    objective="objective",
    why="",
    paused=False,
    remaining_actions=None,
    next_fire=None,
    last_fire=None,
    fires=0,
):
    return _NS(
        schedule=_NS(
            spec=_NS(
                cron_expressions=[cron] if cron else [],
                intervals=[_NS(every=timedelta(seconds=every_seconds))] if every_seconds else [],
            ),
            state=_NS(paused=paused, remaining_actions=remaining_actions),
            # revise_wake rebuilds the whole Schedule, so it reads the current
            # policy too — an incomplete fake silently exercises the error path.
            policy=_NS(overlap="SKIP"),
            action=_NS(
                args=[{"objective": objective, "why": why}],
                workflow="WakeWorkflow",
                id="wake:agent:main:web:user:u1:review",
                task_queue="agent-loop",
            ),
        ),
        info=_NS(
            next_action_times=[next_fire] if next_fire else [],
            recent_actions=[_NS(scheduled_at=last_fire)] if last_fire else [],
            num_actions=fires,
        ),
    )


class FakeScheduleHandle:
    def __init__(self, client, wake_id):
        self._client, self._wake_id = client, wake_id

    async def describe(self):
        if self._client.describe_error:
            raise RuntimeError(self._client.describe_error)
        return self._client.descriptions[self._wake_id]

    async def update(self, update):
        self._client.updated.append((self._wake_id, update.schedule))

    async def delete(self):
        self._client.deleted.append(self._wake_id)


class FakeTemporalClient:
    """Enough of temporalio's Client to drive every wake handler."""

    def __init__(self, schedule_ids=(), describe_error=None, create_error=None):
        self.created = {}
        self.updated = []
        self.deleted = []
        self.descriptions = {}
        self.schedule_ids = list(schedule_ids)
        self.describe_error = describe_error
        self.create_error = create_error

    async def create_schedule(self, wake_id, schedule):
        if self.create_error:
            raise self.create_error
        self.created[wake_id] = schedule

    def get_schedule_handle(self, wake_id):
        return FakeScheduleHandle(self, wake_id)

    async def list_schedules(self):
        # `async def` returning an async iterable, with the await OUTSIDE the for —
        # list_wakes awaits first and then iterates, and doing it the other way was
        # a real bug. A fake that got this wrong would hide that.
        async def gen():
            for wake_id in self.schedule_ids:
                yield _NS(id=wake_id)

        return gen()


class FakeHub:
    """Stands in for the mcp_hub module's four subscription functions."""

    def __init__(self, subscriptions=None, error=None, cancel_result=True):
        self.subscriptions = list(subscriptions or [])
        self.armed = []
        self.revised = []
        self.cancelled = []
        self.error = error
        self.cancel_result = cancel_result

    def _maybe_raise(self):
        if self.error:
            raise self.error

    async def arm_subscription(self, wake_id, event_key, invoke_id, objective):
        self._maybe_raise()
        self.armed.append(
            {
                "wake_id": wake_id,
                "event_key": event_key,
                "invoke_id": invoke_id,
                "objective": objective,
            }
        )

    async def list_subscriptions(self):
        self._maybe_raise()
        return self.subscriptions

    async def revise_subscription(self, wake_id, event_key=None, objective=None):
        self._maybe_raise()
        self.revised.append({"wake_id": wake_id, "event_key": event_key, "objective": objective})
        return True

    async def cancel_subscription(self, wake_id):
        self._maybe_raise()
        self.cancelled.append(wake_id)
        return self.cancel_result


# ----------------------------------------------------------------------- fixtures


def make_ctx(temporal_client=None, session_key=SESSION_KEY) -> ToolContext:
    return ToolContext(
        pool=None,
        session_key=session_key,
        fs_path="/session/x",
        session_dir="/session/x",
        holder_id="holder",
        tool_call_id="tc-1",
        summary_provider=None,
        summary_model="model",
        heartbeat_interval_seconds=1.0,
        lease_ttl_seconds=1.0,
        temporal_client=temporal_client,
    )


@pytest.fixture
def hub(monkeypatch):
    """Install a FakeHub on the module attribute tools_wake actually reaches for.

    Patching `tools_wake.mcp_hub.<fn>` rather than the handler matters: the module
    holds a reference to the mcp_hub module and calls through it at request time.
    """
    fake = FakeHub()
    for name in (
        "arm_subscription",
        "list_subscriptions",
        "revise_subscription",
        "cancel_subscription",
    ):
        monkeypatch.setattr(f"tenant_worker.tools_wake.mcp_hub.{name}", getattr(fake, name))
    return fake


@pytest.fixture(autouse=True)
def fixed_deployment(monkeypatch):
    """_TENANT_SLUG and _TASK_QUEUE are read at IMPORT, not at call time, so setting
    os.environ here would do nothing. The module attributes are the lever."""
    monkeypatch.setattr(tools_wake, "_TENANT_SLUG", "acme")
    monkeypatch.setattr(tools_wake, "_TASK_QUEUE", "agent-loop")


# ------------------------------------------------------------------ pure helpers


@pytest.mark.parametrize("name", ["weekly-review", "a", "a1", "x" * 60])
def test_name_accepts_a_well_formed_identity(name):
    assert tools_wake._name({"name": name}) == name


def test_name_is_lowercased_and_trimmed():
    assert tools_wake._name({"name": "  Weekly-Review  "}) == "weekly-review"


@pytest.mark.parametrize("name", ["", "   ", "-leading", "has space", "has_underscore", "x" * 61])
def test_name_rejects_anything_that_is_not_an_identity(name):
    with pytest.raises(ValueError):
        tools_wake._name({"name": name})


def test_name_is_required():
    with pytest.raises(ValueError, match="requires a short 'name'"):
        tools_wake._name({})


def test_own_id_accepts_both_kinds_of_wake_and_nothing_else():
    ctx = make_ctx()
    assert tools_wake._own_id(ctx, f"wake:{SCOPE}:review") == f"wake:{SCOPE}:review"
    assert tools_wake._own_id(ctx, f"watch:{SCOPE}:dining") == f"watch:{SCOPE}:dining"
    # Another user's wake, however it is spelled.
    with pytest.raises(ValueError, match="does not belong to you"):
        tools_wake._own_id(ctx, "wake:someone-else:review")


def test_rfc3339_normalises_to_utc_seconds():
    assert tools_wake._rfc3339("2026-10-08T15:00:00Z", "at") == "2026-10-08T15:00:00Z"
    # A naive timestamp is read as UTC rather than rejected or shifted.
    assert tools_wake._rfc3339("2026-10-08T15:00:00", "at") == "2026-10-08T15:00:00Z"
    # An offset is converted, not preserved — Go's time.Time unmarshal wants Z.
    assert tools_wake._rfc3339("2026-10-08T17:00:00+02:00", "at") == "2026-10-08T15:00:00Z"


def test_rfc3339_rejects_nonsense():
    with pytest.raises(ValueError, match="not a valid ISO-8601"):
        tools_wake._rfc3339("next tuesday", "at")


def test_trigger_spec_for_a_one_shot_ends_after_a_single_fire():
    fire_at = datetime.now(timezone.utc) + timedelta(hours=1)
    spec, cadence, one_shot = tools_wake._trigger_spec({"at": fire_at.isoformat()})
    assert one_shot is True
    assert cadence.startswith("once at ")
    assert spec.end_at - spec.start_at == timedelta(seconds=1)


def test_trigger_spec_refuses_a_time_in_the_past():
    past = datetime.now(timezone.utc) - timedelta(minutes=1)
    with pytest.raises(ValueError, match="in the past"):
        tools_wake._trigger_spec({"at": past.isoformat()})


def test_trigger_spec_for_cron_and_interval_are_both_recurring():
    spec, cadence, one_shot = tools_wake._trigger_spec({"cron": "0 9 * * MON"})
    assert one_shot is False and "MON" in cadence and spec.cron_expressions == ["0 9 * * MON"]

    spec, cadence, one_shot = tools_wake._trigger_spec({"every_seconds": 900})
    assert one_shot is False and cadence == "every 900s"
    assert spec.intervals[0].every == timedelta(seconds=900)


@pytest.mark.parametrize("every", [0, -5, 0.0])
def test_trigger_spec_rejects_a_non_positive_interval(every):
    # 0 matters as much as -5: it is falsy, so a truthiness check would fall through
    # and tell a model that did supply an interval that it supplied none.
    with pytest.raises(ValueError, match="must be positive"):
        tools_wake._trigger_spec({"every_seconds": every})


def test_trigger_spec_rejects_a_non_numeric_interval():
    with pytest.raises(ValueError, match="must be a number"):
        tools_wake._trigger_spec({"every_seconds": "soon"})


def test_trigger_spec_needs_a_trigger_at_all():
    with pytest.raises(ValueError, match="needs one of"):
        tools_wake._trigger_spec({})


def test_a_wake_needs_a_temporal_client():
    # The guard that turns "ToolCallActivity built without a client" into a clear
    # message rather than an AttributeError deep inside the SDK.
    with pytest.raises(RuntimeError, match="require a Temporal client"):
        tools_wake._client(make_ctx(temporal_client=None))


# ------------------------------------------------------------------- arm_wake


@pytest.mark.asyncio
async def test_arming_a_time_wake_builds_a_schedule_carrying_the_commitment():
    client = FakeTemporalClient()
    result = await tools_wake.arm_wake(
        {"name": "weekly-review", "objective": "review the week", "why": "user asked", "cron": "0 9 * * SUN"},
        make_ctx(client),
    )

    wake_id = f"wake:{SCOPE}:weekly-review"
    assert result == {"wake_id": wake_id, "armed": True, "cadence": "cron 0 9 * * SUN (UTC)", "one_shot": False}

    action = client.created[wake_id].action
    assert action.workflow == "WakeWorkflow"
    assert action.id == wake_id
    assert action.task_queue == "agent-loop"
    # The objective is what the future turn is seeded from, so it has to survive
    # the round trip into the Schedule's arguments.
    assert action.args[0] == {
        "wake_id": wake_id,
        "session_key": SCOPE,
        "tenant_slug": "acme",
        "objective": "review the week",
        "why": "user asked",
    }


@pytest.mark.asyncio
async def test_a_one_shot_is_a_schedule_that_runs_once():
    client = FakeTemporalClient()
    fire_at = datetime.now(timezone.utc) + timedelta(hours=2)
    await tools_wake.arm_wake({"name": "nudge", "objective": "o", "at": fire_at.isoformat()}, make_ctx(client))

    schedule = client.created[f"wake:{SCOPE}:nudge"]
    # Not a separate timer workflow — a one-shot is a Schedule with one action left,
    # so there is exactly one listing path for time wakes.
    assert schedule.state.remaining_actions == 1
    assert schedule.policy.overlap.name == "SKIP"


@pytest.mark.asyncio
async def test_arming_needs_an_objective():
    with pytest.raises(ValueError, match="non-empty 'objective'"):
        await tools_wake.arm_wake({"name": "x", "cron": "* * * * *"}, make_ctx(FakeTemporalClient()))


@pytest.mark.asyncio
async def test_a_duplicate_name_is_reported_for_the_agent_to_resolve():
    client = FakeTemporalClient(create_error=Exception("schedule already exists"))
    result = await tools_wake.arm_wake(
        {"name": "weekly-review", "objective": "o", "cron": "0 9 * * SUN"}, make_ctx(client)
    )

    # Not raised: the identity is the name the agent chose, so a collision is a real
    # duplicate it has to resolve, and it needs to be told which one.
    assert result["armed"] is False
    assert "weekly-review" in result["note"]
    assert "revise" in result["note"]


@pytest.mark.asyncio
async def test_an_unrelated_schedule_error_is_not_mistaken_for_a_duplicate():
    client = FakeTemporalClient(create_error=Exception("connection refused"))
    with pytest.raises(Exception, match="connection refused"):
        await tools_wake.arm_wake(
            {"name": "x", "objective": "o", "cron": "0 9 * * SUN"}, make_ctx(client)
        )


@pytest.mark.asyncio
async def test_arming_an_event_watch_registers_the_objective_with_the_hub(hub):
    result = await tools_wake.arm_wake(
        {
            "name": "dining-over",
            "objective": "Tell them while there is room in the month to adjust.",
            "on_event": {"event_key": "budget:8f14e45f"},
        },
        make_ctx(FakeTemporalClient()),
    )

    wake_id = f"watch:{SCOPE}:dining-over"
    assert result == {
        "wake_id": wake_id,
        "armed": True,
        "on_event": "budget:8f14e45f",
        "objective": "Tell them while there is room in the month to adjust.",
    }
    # The objective is the whole reason a watch takes one: the notice says what
    # happened, and only the armer can say what to do about it.
    assert hub.armed == [
        {
            "wake_id": wake_id,
            "event_key": "budget:8f14e45f",
            "invoke_id": SCOPE,
            "objective": "Tell them while there is room in the month to adjust.",
        }
    ]


@pytest.mark.asyncio
async def test_a_watch_needs_an_event_key(hub):
    for on_event in ({}, {"event_key": "  "}, ""):
        with pytest.raises(ValueError, match="needs an 'event_key'"):
            await tools_wake.arm_wake(
                {"name": "x", "objective": "o", "on_event": on_event}, make_ctx(FakeTemporalClient())
            )


@pytest.mark.asyncio
async def test_a_hub_that_is_unreachable_fails_the_arm_rather_than_lying(hub):
    from tenant_worker.mcp_hub import McpHubCallError

    hub.error = McpHubCallError("POST /api/subscriptions: connection refused")
    # An arm the agent believes succeeded but which registered nothing is an alert
    # that silently never arrives, so this must not return armed=True.
    with pytest.raises(ValueError, match="could not arm the watch"):
        await tools_wake.arm_wake(
            {"name": "x", "objective": "o", "on_event": {"event_key": "budget:1"}},
            make_ctx(FakeTemporalClient()),
        )


# --------------------------------------------------------------- list_wakes


@pytest.mark.asyncio
async def test_list_returns_both_kinds_of_wake_together(hub):
    client = FakeTemporalClient(schedule_ids=[f"wake:{SCOPE}:review", "wake:someone-else:theirs"])
    client.descriptions[f"wake:{SCOPE}:review"] = make_description(cron="0 9 * * SUN", fires=3)
    hub.subscriptions = [
        {"wake_id": f"watch:{SCOPE}:dining", "event_key": "budget:8f14", "objective": "watch it"},
        {"wake_id": "watch:someone-else:theirs", "event_key": "budget:9", "objective": "not mine"},
    ]

    result = await tools_wake.list_wakes({}, make_ctx(client))

    # To the agent a wake is a wake, so both stores appear in one list — but only
    # this user's, whichever store they came from.
    assert {w["wake_id"] for w in result["wakes"]} == {
        f"wake:{SCOPE}:review",
        f"watch:{SCOPE}:dining",
    }
    assert "note" not in result

    schedule = next(w for w in result["wakes"] if w["wake_id"].startswith("wake:"))
    assert schedule["cadence"] == "0 9 * * SUN" and schedule["fires"] == 3

    watch = next(w for w in result["wakes"] if w["wake_id"].startswith("watch:"))
    assert watch["on_event"] == "budget:8f14" and watch["objective"] == "watch it"


@pytest.mark.asyncio
async def test_list_marks_itself_incomplete_when_the_hub_is_down(hub):
    from tenant_worker.mcp_hub import McpHubCallError

    client = FakeTemporalClient(schedule_ids=[f"wake:{SCOPE}:review"])
    client.descriptions[f"wake:{SCOPE}:review"] = make_description(every_seconds=600)
    hub.error = McpHubCallError("GET /api/subscriptions: timed out")

    result = await tools_wake.list_wakes({}, make_ctx(client))

    # The time wakes must still be there — a down hub has nothing to do with them —
    # but the list cannot pretend it is complete, or the agent arms a duplicate of a
    # watch it already has.
    assert [w["wake_id"] for w in result["wakes"]] == [f"wake:{SCOPE}:review"]
    assert "may be incomplete" in result["note"]


@pytest.mark.asyncio
async def test_list_is_silent_when_no_hub_is_configured(hub):
    from tenant_worker.mcp_hub import McpHubNotConfiguredError

    hub.error = McpHubNotConfiguredError("MCP_HUB_URL is not set")
    result = await tools_wake.list_wakes({}, make_ctx(FakeTemporalClient()))

    # No hub means no watch can exist for this tenant either, so this is the whole
    # truth rather than a gap — unlike the down-hub case above.
    assert result == {"wakes": []}


# ------------------------------------------------ inspect / revise / cancel


@pytest.mark.asyncio
async def test_inspect_reads_a_schedule_from_temporal(hub):
    client = FakeTemporalClient()
    wake_id = f"wake:{SCOPE}:review"
    client.descriptions[wake_id] = make_description(
        cron="0 9 * * SUN",
        objective="review the week",
        why="context",
        next_fire=datetime(2026, 10, 11, 9, tzinfo=timezone.utc),
        last_fire=datetime(2026, 10, 4, 9, tzinfo=timezone.utc),
        fires=2,
    )

    result = await tools_wake.inspect_wake({"wake_id": wake_id}, make_ctx(client))
    assert result["state"] == "armed" and result["cadence"] == "0 9 * * SUN"
    assert result["objective"] == "review the week" and result["fires"] == 2
    assert result["next_fire"] == "2026-10-11T09:00:00+00:00"


@pytest.mark.asyncio
async def test_inspect_reads_a_watch_from_the_hub(hub):
    hub.subscriptions = [
        {"wake_id": f"watch:{SCOPE}:dining", "event_key": "budget:8f14", "objective": "watch it"}
    ]
    result = await tools_wake.inspect_wake({"wake_id": f"watch:{SCOPE}:dining"}, make_ctx(FakeTemporalClient()))

    assert result["on_event"] == "budget:8f14" and result["objective"] == "watch it"
    # Empty on purpose: a watch's why is filled by the engine's notice when it
    # fires, so there is no armer-authored one to report.
    assert result["why"] == ""


@pytest.mark.asyncio
async def test_inspect_reports_an_unknown_watch_rather_than_failing(hub):
    result = await tools_wake.inspect_wake({"wake_id": f"watch:{SCOPE}:nope"}, make_ctx(FakeTemporalClient()))
    assert result["state"] == "unknown" and "no such event watch" in result["note"]


@pytest.mark.asyncio
async def test_revise_rewrites_a_schedule_without_clearing_what_it_did_not_mention(hub):
    client = FakeTemporalClient()
    wake_id = f"wake:{SCOPE}:review"
    client.descriptions[wake_id] = make_description(cron="0 9 * * SUN", objective="old", why="kept")

    result = await tools_wake.revise_wake({"wake_id": wake_id, "objective": "new"}, make_ctx(client))

    assert result == {"wake_id": wake_id, "revised": True}
    _, schedule = client.updated[0]
    # Temporal's update replaces the whole Schedule, so the built one must carry
    # forward everything the caller did not name.
    assert schedule.action.args[0]["objective"] == "new"
    assert schedule.action.args[0]["why"] == "kept"
    assert schedule.action.workflow == "WakeWorkflow"
    assert schedule.spec.cron_expressions == ["0 9 * * SUN"]


@pytest.mark.asyncio
async def test_revise_rewrites_a_watch_through_the_hub(hub):
    result = await tools_wake.revise_wake(
        {"wake_id": f"watch:{SCOPE}:dining", "objective": "reworded"},
        make_ctx(FakeTemporalClient()),
    )

    assert result == {"wake_id": f"watch:{SCOPE}:dining", "revised": True}
    assert hub.revised == [
        {"wake_id": f"watch:{SCOPE}:dining", "event_key": None, "objective": "reworded"}
    ]


@pytest.mark.asyncio
async def test_revise_can_re_point_a_watch_at_a_different_event(hub):
    await tools_wake.revise_wake(
        {"wake_id": f"watch:{SCOPE}:dining", "on_event": {"event_key": "budget:2b3c"}},
        make_ctx(FakeTemporalClient()),
    )
    # The trigger is the one thing that differs between the two kinds of wake, so
    # changing it is the only structural edit a watch takes.
    assert hub.revised[0]["event_key"] == "budget:2b3c"


@pytest.mark.asyncio
async def test_revise_refuses_a_watch_with_nothing_to_change(hub):
    with pytest.raises(ValueError, match="needs a new 'objective'"):
        await tools_wake.revise_wake(
            {"wake_id": f"watch:{SCOPE}:dining", "paused": True}, make_ctx(FakeTemporalClient())
        )
    assert hub.revised == []


@pytest.mark.asyncio
async def test_revise_needs_a_field_for_a_schedule(hub):
    with pytest.raises(ValueError, match="at least one of"):
        await tools_wake.revise_wake({"wake_id": f"wake:{SCOPE}:review"}, make_ctx(FakeTemporalClient()))


@pytest.mark.asyncio
async def test_cancel_deletes_a_schedule_from_temporal(hub):
    client = FakeTemporalClient()
    result = await tools_wake.cancel_wake({"wake_id": f"wake:{SCOPE}:review"}, make_ctx(client))
    assert result == {"wake_id": f"wake:{SCOPE}:review", "cancelled": True}
    assert client.deleted == [f"wake:{SCOPE}:review"]
    assert hub.cancelled == []


@pytest.mark.asyncio
async def test_cancel_removes_a_watch_from_the_hub(hub):
    result = await tools_wake.cancel_wake({"wake_id": f"watch:{SCOPE}:dining"}, make_ctx(FakeTemporalClient()))
    assert result == {"wake_id": f"watch:{SCOPE}:dining", "cancelled": True}
    assert hub.cancelled == [f"watch:{SCOPE}:dining"]


@pytest.mark.asyncio
async def test_cancelling_a_watch_that_was_never_armed_says_so(hub):
    hub.cancel_result = False
    result = await tools_wake.cancel_wake({"wake_id": f"watch:{SCOPE}:nope"}, make_ctx(FakeTemporalClient()))
    # False is a real answer — the agent should not be told a disarm succeeded when
    # nothing was armed under that name.
    assert result["cancelled"] is False


# ------------------------------------------------------------------- manage_wake


@pytest.mark.asyncio
async def test_manage_dispatches_to_each_verb(hub):
    client = FakeTemporalClient(schedule_ids=[f"wake:{SCOPE}:review"])
    client.descriptions[f"wake:{SCOPE}:review"] = make_description(every_seconds=600)

    assert "wakes" in await tools_wake.manage_wake({"action": "list"}, make_ctx(client))
    assert (await tools_wake.manage_wake({"action": "inspect", "wake_id": f"wake:{SCOPE}:review"}, make_ctx(client)))["cadence"] == "every 600s"
    assert (await tools_wake.manage_wake({"action": "cancel", "wake_id": f"wake:{SCOPE}:review"}, make_ctx(client)))["cancelled"] is True


@pytest.mark.asyncio
async def test_manage_refuses_an_unknown_action_rather_than_no_opping(hub):
    with pytest.raises(ValueError, match="unknown action"):
        await tools_wake.manage_wake({"action": "snooze"}, make_ctx(FakeTemporalClient()))
    with pytest.raises(ValueError, match="unknown action"):
        await tools_wake.manage_wake({}, make_ctx(FakeTemporalClient()))
