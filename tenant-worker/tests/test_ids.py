"""ids.py — the ID scheme, hand-mirrored from shared/ids/ids.go.

Every function here is a pure string transform, which makes this the cheapest place
in the package to pin a contract that two languages implement separately. A drift
between them does not fail loudly anywhere — it produces an ID that looks fine and
resolves to the wrong turn.
"""

import pytest

from tenant_worker import ids


def test_activity_and_subagent_ids_nest_under_their_turn():
    assert ids.activity_id("sess-1:turn:1", 3) == "sess-1:turn:1:act:3"
    assert ids.subagent_turn_id("sess-1:turn:1", 2) == "sess-1:turn:1:sub:2"
    # Recursion is the same function applied again — that is the whole design.
    assert (
        ids.subagent_turn_id(ids.subagent_turn_id("sess-1:turn:1", 1), 2)
        == "sess-1:turn:1:sub:1:sub:2"
    )


def test_turn_id_of_tool_call_recovers_the_nesting_owner():
    assert ids.turn_id_of_tool_call("sess-1:turn:1:act:3") == "sess-1:turn:1"
    # ":act:" only ever appears once and terminates the id, so the LAST occurrence
    # is the owning turn — that is what makes mid-turn retrieval work from a
    # handler that only has its own ToolContext.
    assert ids.turn_id_of_tool_call("sess-1:turn:1:sub:1:act:2") == "sess-1:turn:1:sub:1"


def test_turn_id_of_tool_call_rejects_something_that_is_not_one():
    with pytest.raises(ValueError, match=":act:"):
        ids.turn_id_of_tool_call("sess-1:turn:1")


def test_session_key_of_strips_the_turn():
    assert ids.session_key_of("sess-1:turn:4") == "sess-1"
    with pytest.raises(ValueError, match=":turn:"):
        ids.session_key_of("sess-1")


def test_user_scope_is_stable_across_a_users_branches_and_threads():
    # The point of the function: a wake armed in one branch has to be visible from
    # another, so the scope must not carry the branch.
    assert ids.user_scope_of("agent:main:web:user:u1:session:abc") == "agent:main:web:user:u1"
    assert ids.user_scope_of("agent:main:web:user:u1:thread:t9") == "agent:main:web:user:u1"
    # ...and the result is itself a valid canonical session key, which is what lets
    # a fired wake name the session it wakes.
    assert ids.user_scope_of("agent:main:web:user:u1") == "agent:main:web:user:u1"
    # A shared channel has no user to strip back to; the channel IS the scope.
    assert ids.user_scope_of("discord:chan:42") == "discord:chan:42"


def test_session_fs_path_maps_nesting_to_directories():
    assert ids.session_fs_path("sess-1:turn:1") == "/session/sess-1/"
    assert ids.session_fs_path("sess-1:turn:1:sub:1") == "/session/sess-1/sub/1/"
    assert ids.session_fs_path("sess-1:turn:1:sub:1:sub:2") == "/session/sess-1/sub/1/sub/2/"


def test_session_fs_path_treats_non_fs_segments_as_transparent():
    # A checkpoint is not a filesystem boundary: a subagent spawned from one lands
    # where a subagent spawned from the turn directly would.
    assert ids.session_fs_path("sess-1:turn:1:cp:2:sub:1") == "/session/sess-1/sub/1/"
    # A nested reasoning turn has to truncate at ":act:" before mapping. Despite
    # the name this shape really does arrive here — it was found by a real failure.
    assert ids.session_fs_path("sess-1:turn:6:act:1:reason") == "/session/sess-1/"


def test_session_fs_path_rejects_an_unknown_segment():
    with pytest.raises(ValueError, match="unexpected turn_id segment"):
        ids.session_fs_path("sess-1:turn:1:bogus:2")


def test_session_fs_path_rejects_something_that_is_not_a_turn_id():
    with pytest.raises(ValueError, match=":turn:"):
        ids.session_fs_path("sess-1")
