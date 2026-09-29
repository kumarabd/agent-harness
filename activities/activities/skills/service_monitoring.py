"""Service monitoring policy: Grafana evidence plus a durable intention."""

from __future__ import annotations

from .base import SkillDefinition, register

SYSTEM_PROMPT = """Advance the confirmed service-monitoring request.
Use Grafana as the evidence source. Discover its read-only search, query and
dashboard tools, inspect the actual service, and select a health signal supported
by those results. Do not change Grafana alerts or other external configuration.
If evidence is absent or the target is ambiguous, stop and explain the blocker.
Create a condition intention using the observed Grafana server/tool and an explicit
predicate matching the approved request. Condition intentions fire once; do not
claim continuous recurring monitoring. If the requested commitment cannot be
represented, stop and explain it. Make one tool call per response.
Only a successful create_intention after Grafana evidence completes this step.
You do not change the user's selected mode or ask/deliver directly to the user."""

READ_TOOLS = frozenset(
    {
        "search_dashboards",
        "get_dashboard_by_uid",
        "get_dashboard_summary",
        "list_datasources",
        "get_datasource_by_name",
        "get_datasource_by_uid",
        "query_prometheus",
        "query_loki_logs",
        "list_alert_rules",
        "get_alert_rule_by_uid",
        "list_prometheus_metric_names",
        "list_prometheus_label_names",
        "list_prometheus_label_values",
    }
)


def validate(tool: str, args: dict, content: str, history: list[dict]) -> None:
    if tool != "create_intention":
        return
    evidence = [
        h
        for h in history
        if h["request"].get("tool") in READ_TOOLS
        and h.get("response") is not None
        and not h["response"].get("error")
        and not h["response"].get("isError")
    ]
    if not evidence:
        raise ValueError("Grafana evidence is required before arming monitoring")
    if args.get("kind") != "condition":
        raise ValueError("this monitoring step supports a one-shot condition intention")
    probe = args.get("probe") or {}
    allowed = {h["request"]["server"] + "/" + h["request"]["tool"] for h in evidence}
    if probe.get("tool") not in allowed:
        raise ValueError(
            "monitoring probe must use the Grafana tool actually inspected"
        )


def complete(content: str, current: dict, history: list[dict]) -> bool:
    # create_intention has two non-error success shapes: a fresh arm
    # ({"intention_id", "armed": True}) and a dedup hit against an existing
    # intention with a matching objective ({"intention_id", "note": ...}, no
    # "armed" key). Both mean the desired external state already holds —
    # only a missing intention_id or an explicit error means it doesn't.
    result = current.get("response") or {}
    return (
        current["request"].get("tool") == "create_intention"
        and bool(result.get("intention_id"))
        and not result.get("error")
    )


async def create_intention(args, ctx):
    from ..tools_intention import create_intention as execute

    return await execute(args, ctx)


register(
    SkillDefinition(
        name="service_monitoring",
        description="Inspect Grafana and arm an approved service-health intention.",
        system_prompt=SYSTEM_PROMPT,
        aliases=("service monitoring", "service_monitoring"),
        backend="grafana",
        read_tools=READ_TOOLS,
        write_tools=frozenset(),
        native_tools={"create_intention": create_intention},
        validate=validate,
        complete=complete,
    )
)
