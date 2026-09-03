"""Shared metric helpers for the pre-LLM request pipeline
(docs/components/request-pipeline.md).

Why this module exists
----------------------
Every pipeline activity (ClassifyRequest, ResolveOpenPlan, MemoryRetrieve,
ToolDiscover, SkillDiscover) is best-effort and short on its own, but they run
*serially before the first model token* — so their combined cost is what a
turn's time-to-first-token is made of.

**Latency is already measured.** The Temporal SDK core emits
``temporal_activity_execution_latency`` (a histogram, unit = *milliseconds*,
label ``activity_type``) for every activity, scraped from the tenant worker's
own ``/metrics``. Query THAT for per-activity timing — e.g.
``temporal_activity_execution_latency{activity_type="SkillDiscover"}`` —
not a hand-rolled histogram, and not the ``_seconds`` variant (that name is
only emitted by the Go gateway, which sets ``durations_as_seconds=true``).

What the SDK metric can't show is the *semantic* outcome: whether
MemoryRetrieve found anything, whether ResolveOpenPlan resolved to
continue / supersede / none. Those decide lane routing and prompt content
downstream, so `observe_outcome` adds one cheap counter per activity carrying
just that.
"""

from __future__ import annotations

import functools
from collections.abc import Awaitable, Callable
from typing import Any, TypeVar

from temporalio import activity

_T = TypeVar("_T")

# Seconds-appropriate histogram boundaries for the three hand-rolled
# ``*_latency_seconds`` metrics (classify / model_call / tool_call). Temporal
# core's default boundaries are millisecond-oriented, so a value recorded in
# seconds collapses into the lowest bucket and every percentile reads the
# same. Applied by name in tenant_worker.py's PrometheusConfig
# (histogram_bucket_overrides) — keep the two in sync.
LATENCY_BUCKETS_SECONDS: tuple[float, ...] = (
    0.05,
    0.1,
    0.25,
    0.5,
    1.0,
    2.0,
    4.0,
    8.0,
    15.0,
    30.0,
    60.0,
    120.0,
)

SECONDS_LATENCY_METRICS: tuple[str, ...] = (
    "classify_request_latency_seconds",
    "model_call_latency_seconds",
    "tool_call_latency_seconds",
    "prompt_assemble_latency_seconds",
)


def _outcome_of(result: Any) -> str:
    """Best-effort semantic label for what an activity returned.

    - ``SubsystemResult`` (MemoryRetrieve / ToolDiscover / SkillDiscover)
      -> its ``.status`` (``ok`` | ``empty`` | ``error``).
    - ``ResolveOpenPlanResult`` -> ``supersede`` | ``continue`` | ``none``
      (no running plan to resolve against).
    - anything else -> ``ok``.
    """
    if hasattr(result, "status") and isinstance(result.status, str) and result.status:
        return result.status
    if hasattr(result, "should_continue") and hasattr(result, "plan_id"):
        if getattr(result, "supersede", False):
            return "supersede"
        if result.should_continue:
            return "continue"
        return "none"
    return "ok"


def observe_outcome(
    counter_name: str,
) -> Callable[[Callable[..., Awaitable[_T]]], Callable[..., Awaitable[_T]]]:
    """Decorate a pipeline activity's ``__call__`` to emit
    ``<counter_name>{outcome=...}`` once per invocation. ``outcome`` is the
    returned object's semantic status (see `_outcome_of`), or ``error`` if the
    activity raised (the exception still propagates unchanged).

    Ordering: put this *below* ``@activity.defn`` so the decorator applies
    first and Temporal registers the wrapped function::

        @activity.defn(name="ComposeSkill")
        @observe_outcome("compose_skill_total")
        async def __call__(self, input): ...

    ``functools.wraps`` keeps ``__name__``/``__wrapped__``/annotations, which
    is what ``temporalio.activity`` reads for the signature.
    """

    def decorate(fn: Callable[..., Awaitable[_T]]) -> Callable[..., Awaitable[_T]]:
        @functools.wraps(fn)
        async def wrapper(*args: Any, **kwargs: Any) -> _T:
            outcome = "error"
            try:
                result = await fn(*args, **kwargs)
                outcome = _outcome_of(result)
                return result
            finally:
                activity.metric_meter().with_additional_attributes(
                    {"outcome": outcome}
                ).create_counter(counter_name).add(1)

        return wrapper

    return decorate
