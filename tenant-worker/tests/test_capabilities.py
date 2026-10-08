"""capabilities.py — the declarative table every turn's tool set is built from.

The value here is mostly in the cross-module invariants. A capability naming a
handler that does not exist, or a handler nobody offers, is a silent hole: the model
is never offered the tool, or is offered one that fails on call. Neither shows up
until someone notices a tool missing in production.
"""

import pytest

from tenant_worker import capabilities, tools
from tenant_worker.capabilities import Capability, Layer, TurnKind


def test_turn_kind_maps_from_the_subagent_flag():
    assert capabilities.turn_kind_of(False) is TurnKind.REASONING
    assert capabilities.turn_kind_of(True) is TurnKind.SUBAGENT


# ------------------------------------------------------------- invariants


def test_every_capabilitys_handler_ref_names_a_real_handler():
    for name, ref in capabilities.HANDLER_REFS.items():
        assert ref in tools._HANDLERS, f"capability {name!r} names handler {ref!r}, which does not exist"


def test_every_registered_handler_is_offered_by_some_capability():
    claimed = set(capabilities.HANDLER_REFS.values())
    orphans = sorted(set(tools._HANDLERS) - claimed)
    # A handler nothing offers is either a tool that silently disappeared from every
    # turn, or dead code — both worth failing on.
    assert orphans == [], f"handlers registered but never offered: {orphans}"


def test_capability_names_are_unique():
    names = [c.name for c in capabilities.CAPABILITIES]
    assert len(names) == len(set(names))
    assert set(capabilities.BY_NAME) == set(names)


@pytest.mark.parametrize("kind", list(TurnKind))
def test_schema_for_never_reaches_for_a_missing_schema(kind):
    # schema_for indexes _SCHEMA_BY_NAME directly, so a capability without a schema
    # is a KeyError at the start of every turn of that kind.
    schemas = capabilities.schema_for(kind)
    assert schemas
    for schema in schemas:
        assert schema["function"]["name"]


def test_schema_for_can_force_include_a_capability_outside_the_turn_kind():
    # deliver_reply is in no turn's default set — only turn.go's recovery round.
    default = {s["function"]["name"] for s in capabilities.schema_for(TurnKind.REASONING)}
    assert "deliver_reply" not in default

    forced = {
        s["function"]["name"]
        for s in capabilities.schema_for(TurnKind.REASONING, also=frozenset({"deliver_reply"}))
    }
    assert "deliver_reply" in forced


def test_call_tool_is_never_offered_to_the_model():
    # Internal-only since per-task resolution: ToolCall proxies a resolved dispatch
    # through it directly. It keeps a handler_ref for its timing profile, so the
    # empty turn_kinds is the only thing keeping it out of the model's view.
    assert capabilities.BY_NAME["call_tool"].turn_kinds == frozenset()
    for kind in TurnKind:
        assert "call_tool" not in {s["function"]["name"] for s in capabilities.schema_for(kind)}


# ---------------------------------------------------------- minting resolved


def _row(server="finance", tool="budget_status", schema=None, description="what it does"):
    return (f"{server}/{tool} — {description}", {"server": server, "tool": tool, "input_schema": schema or {"type": "object"}})


def test_minting_a_resolved_tool_carries_its_schema_and_description():
    minted = capabilities.mint_resolved([_row()])
    assert [c.name for c in minted] == ["budget_status"]
    assert minted[0].schema["function"]["description"] == "what it does"


def test_minting_skips_rows_that_cannot_be_called():
    # "Advisory, not restrictive": an unusable row is simply not offered directly,
    # and the model still has search_tools.
    assert capabilities.mint_resolved([("no metadata", None)]) == []
    assert capabilities.mint_resolved([("x", {"server": "s"})]) == []
    assert capabilities.mint_resolved([("x", {"server": "s", "tool": "t", "input_schema": "not a dict"})]) == []


def test_minting_skips_the_shell_server():
    # A shell hit routes through the real shell_exec capability. Minting it would
    # shadow that by name and then fail with "Unknown server: shell" on every call.
    assert capabilities.mint_resolved([_row(server="shell", tool="shell_exec")]) == []


def test_minting_keeps_the_last_resolved_tools_when_more_arrive_than_fit():
    rows = [_row(tool=f"t{n}") for n in range(capabilities.MAX_RESOLVED + 3)]
    minted = capabilities.mint_resolved(rows)
    assert len(minted) == capabilities.MAX_RESOLVED
    # rows is seq-ordered and a mid-turn search_tools appends, so the model's own
    # deliberate follow-up discovery outranks the pre-turn guess.
    assert minted[-1].name == f"t{capabilities.MAX_RESOLVED + 2}"
    assert "t0" not in {c.name for c in minted}


def test_a_name_collision_falls_back_to_a_server_qualified_one():
    minted = capabilities.mint_resolved([_row(server="a", tool="search"), _row(server="b", tool="search")])
    assert [c.name for c in minted] == ["search", "b_search"]


def test_mint_name_sanitises_and_never_returns_empty():
    assert capabilities._mint_name("s", "weird.name/v2", set()) == "weird_name_v2"
    assert capabilities._mint_name("s", "!!!", set()) == "_" * 3
    assert capabilities._mint_name("s", "", set()) == "tool"
    assert len(capabilities._mint_name("s", "x" * 200, set())) == 64


def test_mint_name_keeps_trying_until_it_is_unique():
    taken = {"search", "a_search"}
    assert capabilities._mint_name("a", "search", taken) == "a_search_2"
