"""llm.py — the model-facing tool schemas.

These are data, not logic, which is exactly why they need pinning: a schema is the
only thing telling the model a tool exists and how to call it, and it is checked by
nothing at runtime. Every shape asserted here has been changed by hand at least once,
and a wrong one fails as a tool the model never calls rather than as an error.
"""

import pytest

from tenant_worker import capabilities, llm


def schema(name: str) -> dict:
    return llm._SCHEMA_BY_NAME[name]


# ------------------------------------------------------------- well-formedness


def test_every_schema_is_a_well_formed_function_definition():
    assert llm._SCHEMA_BY_NAME
    for name, entry in llm._SCHEMA_BY_NAME.items():
        assert entry["type"] == "function"
        assert entry["function"]["name"] == name
        assert entry["function"]["description"].strip()
        assert entry["function"]["parameters"]["type"] == "object"


def test_schema_names_are_unique_and_reachable_by_name():
    names = [t["function"]["name"] for t in llm.TOOLS_SCHEMA]
    assert len(names) == len(set(names))
    assert set(names) <= set(llm._SCHEMA_BY_NAME)


def test_every_required_property_is_actually_declared():
    # A `required` naming a property that is not in `properties` is invalid JSON
    # Schema; providers vary between rejecting it and silently ignoring the field.
    for name, entry in llm._SCHEMA_BY_NAME.items():
        fn = entry["function"]
        declared = set(fn["parameters"].get("properties", {}))
        for field in fn["parameters"].get("required", []):
            assert field in declared, f"{name}: required {field!r} is not declared"


def test_capabilities_schema_for_hands_back_these_same_dicts():
    # schema_for indexes _SCHEMA_BY_NAME directly, so anything it selects has to be
    # one of these objects and not a copy that could drift.
    for kind in capabilities.TurnKind:
        for entry in capabilities.schema_for(kind):
            name = entry["function"]["name"]
            # spawn_subagent is the one legitimate swap: a subagent turn gets the
            # nested variant (it needs delegated_scope/kept_work), a reasoning turn
            # the flat one. Everything else must be the same object.
            if name in llm._SCHEMA_BY_NAME and name != "spawn_subagent":
                assert entry is llm._SCHEMA_BY_NAME[name]


# ------------------------------------------------------------------- arm_wake


def test_arm_wake_declares_one_shape_for_both_triggers():
    fn = schema("arm_wake")["function"]
    params = fn["parameters"]

    # One shape, two triggers: a name and an objective either way. Deliberately NOT
    # an anyOf — an event watch needs the objective just as much as a time wake,
    # because a notice says what happened and not what to do about it.
    assert params["required"] == ["name", "objective"]
    assert "anyOf" not in params
    assert set(params["properties"]) >= {
        "name", "objective", "why", "at", "cron", "every_seconds", "on_event",
    }


def test_arm_wake_on_event_takes_an_event_key():
    on_event = schema("arm_wake")["function"]["parameters"]["properties"]["on_event"]
    assert on_event["type"] == "object"
    assert on_event["required"] == ["event_key"]
    assert "event_key" in on_event["properties"]


def test_arm_wake_tells_the_model_to_prefer_a_watch_over_polling():
    description = schema("arm_wake")["function"]["description"]
    # The selection rule is the whole design of the tool, and it lives only here.
    assert "on_event" in description
    assert "WATCH OVER POLLING" in description


def test_arm_wake_says_the_objective_is_required_for_both():
    description = schema("arm_wake")["function"]["description"]
    assert "objective" in description
    # Guards against drifting back to "only a time wake needs one".
    assert "Do NOT supply an objective" not in description


def test_why_is_documented_as_time_wakes_only():
    why = schema("arm_wake")["function"]["parameters"]["properties"]["why"]
    # A watch's why is the engine's notice, which arrives when it fires — there is
    # no armer-authored one, so the model should not be told to write it.
    assert "Time wakes only" in why["description"]


# ----------------------------------------------------------------- manage_wake


def test_manage_wake_offers_exactly_the_four_verbs():
    params = schema("manage_wake")["function"]["parameters"]
    assert params["properties"]["action"]["enum"] == ["list", "inspect", "revise", "cancel"]
    assert params["required"] == ["action"]


def test_manage_wake_exposes_re_pointing_a_watch():
    props = schema("manage_wake")["function"]["parameters"]["properties"]
    # The trigger is the only thing that differs between the two kinds of wake, so
    # re-pointing is the one structural edit a watch takes — and it needs a field.
    assert props["on_event"]["required"] == ["event_key"]
    assert "wake_id" in props


def test_manage_wake_says_it_covers_both_kinds():
    fn = schema("manage_wake")["function"]
    assert "event watches" in fn["description"]
    # The id is the one thing that differs by store, so the field says the agent
    # never has to reason about which store a wake lives in.
    assert "which kind" in fn["parameters"]["properties"]["wake_id"]["description"]


def test_spawn_subagent_swaps_to_a_nested_variant_on_a_subagent_turn():
    reasoning = {
        s["function"]["name"]: s for s in capabilities.schema_for(capabilities.TurnKind.REASONING)
    }["spawn_subagent"]
    nested = {
        s["function"]["name"]: s for s in capabilities.schema_for(capabilities.TurnKind.SUBAGENT)
    }["spawn_subagent"]

    assert reasoning is llm._SCHEMA_BY_NAME["spawn_subagent"]
    assert nested is not reasoning
    # A subagent has to say what it is keeping, so the work it delegates cannot be
    # silently unbounded.
    assert "delegated_scope" in nested["function"]["parameters"]["properties"]
