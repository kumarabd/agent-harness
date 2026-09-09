"""Shared metric config — the seconds-oriented histogram boundaries and the
list of metric names they apply to.

The Temporal SDK core emits ``temporal_activity_execution_latency`` (a
histogram, unit = *milliseconds*) for every activity, but with core *default*
boundaries (``le`` 50/100/500/1000/5000/10000/60000) that floor every sub-50ms
activity and inflate the top bucket toward 60s — so ``histogram_quantile`` on
it is unusable and only its ``_sum``/``_count`` mean is trustworthy. That
bucket override can't be applied to it without distorting every *other* SDK
duration metric.

So the handful of hand-rolled ``*_latency_seconds`` histograms (a provider
round-trip, prompt assembly, a tool call) get the widened
``LATENCY_BUCKETS_SECONDS`` boundaries instead — applied by name in
tenant_worker.py's PrometheusConfig via ``histogram_bucket_overrides``.
"""

from __future__ import annotations

# Seconds-appropriate histogram boundaries. Applied by name in
# tenant_worker.py's PrometheusConfig (histogram_bucket_overrides).
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

# The hand-rolled ``*_latency_seconds`` histograms that record in seconds and
# therefore need the widened boundaries above (model_call.py, tool_call.py).
SECONDS_LATENCY_METRICS: tuple[str, ...] = (
    "model_call_latency_seconds",
    "tool_call_latency_seconds",
    "prompt_assemble_latency_seconds",
)
