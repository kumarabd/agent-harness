"""wake.py — the one activity every kind of wake goes through.

This is the file where the bug actually was: `WakeSession` set
`id_conflict_policy=USE_EXISTING` but no reuse policy, so it inherited Temporal's
`ALLOW_DUPLICATE_FAILED_ONLY`. Those two policies cover *disjoint* cases — conflict
while an execution is RUNNING, reuse once it has CLOSED — and the coordinator exits
cleanly on idle TTL. So waking a session that had gone idle, which is the ordinary
case rather than the rare one, was refused outright, and nothing in the package could
have noticed.

Both policies are asserted here for that reason, together with the fact that they are
not interchangeable.
"""

import asyncio
import os

import pytest

from tenant_worker.types import WakeSessionInput
from tenant_worker.wake import WakeSessionActivity


class FakeTemporalClient:
    def __init__(self):
        self.last: dict = {}

    async def start_workflow(self, workflow, arg, **kwargs):
        self.last = {"workflow": workflow, "arg": arg, **kwargs}


@pytest.fixture
def client(monkeypatch):
    monkeypatch.setenv("TENANT_SLUG", "acme")
    monkeypatch.setenv("TEMPORAL_WORKFLOW_TASK_QUEUE", "agent-loop")
    return FakeTemporalClient()


async def call_activity(client, input):
    """`__call__` carries @activity.defn, which attaches metadata to the function
    rather than wrapping it — so the bound method is directly awaitable."""
    activity = WakeSessionActivity(pool=None, temporal_client=client)
    return await activity.__call__(input)


@pytest.mark.asyncio
async def test_a_wake_signal_starts_the_coordinator_as_a_wake_payload(client):
    await call_activity(
        client,
        WakeSessionInput(
            wake_id="wake:user_1:review",
            session_key="user_1",
            objective="review the week",
            why="you asked on Sunday",
        ),
    )

    last = client.last
    assert last["workflow"] == "CoordinatorWorkflow"
    # Addressed by the session key, so a running coordinator is reused and a
    # stopped one is started — the whole reason SignalWithStart is the primitive.
    assert last["id"] == "user_1"
    assert last["task_queue"] == "agent-loop"
    assert last["start_signal"] == "Wake"
    assert last["start_signal_args"] == [
        {"wake_id": "wake:user_1:review", "objective": "review the week", "why": "you asked on Sunday"}
    ]
    assert last["arg"] == {"session_key": "user_1", "tenant_slug": "acme"}


@pytest.mark.asyncio
async def test_a_wake_can_restart_a_coordinator_that_exited_cleanly_on_idle_ttl(client):
    await call_activity(client, WakeSessionInput(wake_id="w", session_key="user_1", objective="o"))

    # THE REGRESSION THIS FILE EXISTS FOR. The coordinator returns nil on idle TTL,
    # and Temporal's default reuse policy allows an id back only if the previous run
    # FAILED — so without ALLOW_DUPLICATE this call is refused, and proactive wakes
    # only work while the coordinator happens to still be running.
    assert client.last["id_reuse_policy"].name == "ALLOW_DUPLICATE"


@pytest.mark.asyncio
async def test_a_wake_folds_into_a_coordinator_that_is_still_running(client):
    await call_activity(client, WakeSessionInput(wake_id="w", session_key="user_1", objective="o"))

    # The other half of the pair, and not a substitute for it: reuse policy covers a
    # CLOSED execution, conflict policy a RUNNING one. Both are needed.
    assert client.last["id_conflict_policy"].name == "USE_EXISTING"


@pytest.mark.asyncio
async def test_the_two_policies_are_set_independently(client):
    await call_activity(client, WakeSessionInput(wake_id="w", session_key="user_1", objective="o"))

    policies = {k for k in client.last if k.startswith("id_")}
    # If a future edit collapses these into one, this fails — they are not the same
    # setting spelled twice, which is exactly the mistake that shipped.
    assert policies == {"id_reuse_policy", "id_conflict_policy"}


@pytest.mark.asyncio
async def test_the_deployment_values_are_read_when_the_activity_is_built(monkeypatch):
    # Unlike tools_wake, this reads env in __init__ — so setting it here is enough,
    # and the activity is safe to construct once per worker process.
    monkeypatch.setenv("TENANT_SLUG", "other")
    monkeypatch.setenv("TEMPORAL_WORKFLOW_TASK_QUEUE", "custom-queue")
    client = FakeTemporalClient()

    await call_activity(client, WakeSessionInput(wake_id="w", session_key="s", objective="o"))

    assert client.last["task_queue"] == "custom-queue"
    assert client.last["arg"]["tenant_slug"] == "other"
