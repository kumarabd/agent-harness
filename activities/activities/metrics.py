"""Shared metric helpers for the pre-LLM request pipeline
(docs/components/request-pipeline.md).

Why this module exists
----------------------
Every pipeline activity (ClassifyRequest, ResolveOpenPlan, MemoryRetrieve,
ToolDiscover, SkillDiscover) is best-effort and short on its own, but they run
*serially before the first model token* — so their combined cost is what a
turn's time-to-first-token is made of.

**Latency.** The Temporal SDK core emits ``temporal_activity_execution_latency``
(a histogram, unit = *milliseconds*, label ``activity_type``) for every
activity — but with the core *default* boundaries (``le`` 50/100/500/1000/
5000/10000/60000), which floor every sub-50ms activity and inflate anything in
the top bucket toward 60s, so ``histogram_quantile`` on it is unusable and
only its ``_sum``/``_count`` mean is trustworthy. The bucket override can't be
applied to it without also distorting every *other* SDK duration metric. So
the retrieval fan-out activities (MemoryRetrieve / ToolDiscover / SkillDiscover)
get their own ``*_latency_seconds`` histogram via `observe_outcome`, with the
widened `LATENCY_BUCKETS_SECONDS` boundaries — real p50/p95/p99 for the three
activities whose serial cost is the turn's time-to-first-token.

`observe_outcome` also carries the *semantic* outcome the SDK metric can't
show: whether MemoryRetrieve found anything, whether ResolveOpenPlan resolved
to continue / supersede / none — as an ``{outcome}`` attribute on both the
counter and the histogram.
"""

from __future__ import annotations

import functools
import time
from collections.abc import Awaitable, Callable
from typing import Any, TypeVar

from temporalio import activity

_T = TypeVar("_T")

# Seconds-appropriate histogram boundaries for our hand-rolled
# ``*_latency_seconds`` metrics. Temporal core's default boundaries are
# millisecond-oriented, so a value recorded in seconds collapses into the
# lowest bucket and every percentile reads the same. Applied by name in
# tenant_worker.py's PrometheusConfig (histogram_bucket_overrides) — keep the
# two in sync (both import from here, so the "sync" is just this list).
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

# Counters emitted by `observe_outcome`. Each also gets a paired
# ``<name without _total>_latency_seconds`` histogram (same {outcome} attr) —
# the SDK's own ``temporal_activity_execution_latency`` keeps coarse
# ms-default buckets that make its percentiles unusable, so the retrieval
# fan-out activities carry their own seconds histogram instead.
_OUTCOME_COUNTERS: tuple[str, ...] = (
    "memory_retrieve_total",
    "skill_discover_total",
    "tool_discover_total",
)


def _latency_metric_name(counter_name: str) -> str:
    """``memory_retrieve_total`` -> ``memory_retrieve_latency_seconds``."""
    return counter_name.removesuffix("_total") + "_latency_seconds"


SECONDS_LATENCY_METRICS: tuple[str, ...] = (
    "classify_request_latency_seconds",
    "model_call_latency_seconds",
    "tool_call_latency_seconds",
    "prompt_assemble_latency_seconds",
    "record_skill_phase_latency_seconds",
    *(_latency_metric_name(c) for c in _OUTCOME_COUNTERS),
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
    """Decorate a pipeline activity's ``__call__`` to emit, once per invocation:

    - ``<counter_name>{outcome=...}`` — a counter, and
    - ``<counter_name without _total>_latency_seconds{outcome=...}`` — a
      histogram (seconds; buckets widened in tenant_worker.py via
      `SECONDS_LATENCY_METRICS`).

    ``outcome`` is the returned object's semantic status (see `_outcome_of`), or
    ``error`` if the activity raised (the exception still propagates unchanged).
    The latency is recorded on every path, raise included.

    Ordering: put this *below* ``@activity.defn`` so the decorator applies
    first and Temporal registers the wrapped function::

        @activity.defn(name="SkillDiscover")
        @observe_outcome("skill_discover_total")
        async def __call__(self, input): ...

    ``functools.wraps`` keeps ``__name__``/``__wrapped__``/annotations, which
    is what ``temporalio.activity`` reads for the signature.
    """
    hist_name = _latency_metric_name(counter_name)

    def decorate(fn: Callable[..., Awaitable[_T]]) -> Callable[..., Awaitable[_T]]:
        @functools.wraps(fn)
        async def wrapper(*args: Any, **kwargs: Any) -> _T:
            outcome = "error"
            started = time.monotonic()
            try:
                result = await fn(*args, **kwargs)
                outcome = _outcome_of(result)
                return result
            finally:
                tagged = activity.metric_meter().with_additional_attributes(
                    {"outcome": outcome}
                )
                tagged.create_counter(counter_name).add(1)
                tagged.create_histogram_float(hist_name, unit="s").record(
                    time.monotonic() - started
                )

        return wrapper

    return decorate
