"""Model Registry — docs/components/model-registry.md. Modality x Tier
structure (registry[modality][tier] -> model config). `language` is real,
three tiers (fast/medium/expert); vision/audio/video are placeholders only
— nothing in the current toolset calls them, so resolve() rejects them
rather than pretending to support dispatch that doesn't exist.

Storage: env vars, not Postgres — this is deployment-time configuration,
same category as today's PIONEER_* (an explicitly open question in the
design doc, resolved here in favor of the existing pattern rather than
inventing a new one). Scoped to one OpenAI-compatible provider, matching
llm.py's existing assumption — true multi-provider abstraction (different
request/response shapes, not just different base URLs) is a separately
open, deferred question in the doc, not built ahead of a real second
provider actually needing it.

Backward compatible with a deployment that's only ever set PIONEER_MODEL:
any tier without its own LANGUAGE_<TIER>_MODEL override falls back to
PIONEER_MODEL, so an unconfigured registry behaves exactly like today's
single-model setup — every tier just resolves to the same model.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

_LANGUAGE_TIERS = ("fast", "medium", "expert")
_DEFAULT_TIER = "medium"  # docs/components/model-registry.md, "Resolved: Selection Mechanism" — bootstrap default
_DEFAULT_CONTEXT_WINDOW = 128_000  # conservative fallback if a tier's own context window isn't configured


@dataclass(frozen=True)
class ModelConfig:
    model: str
    context_window: int
    # docs/components/budget-guardrails.md's own dependency: "real cost
    # tracking needs a per-model $/token table, which the model registry is
    # the natural owner of." Three separate rates, not one — providers price
    # input/output tokens differently (often 3-5x apart), and cached-input
    # tokens (a prompt-cache hit) separately again at a further discount from
    # regular input. Defaults to 0.0 (unconfigured/unknown), same honest-scope
    # choice budget-guardrails.md already made for token-count metrics
    # shipping ahead of cost metrics — a 0 rate means "no cost data," not
    # "free."
    input_cost_per_token: float = 0.0
    output_cost_per_token: float = 0.0
    cached_input_cost_per_token: float = 0.0


def default_hint() -> tuple[str, str]:
    """docs/components/model-registry.md, "Resolved: Selection Mechanism" —
    the first ModelCall of a turn has no prior hint, defaults to
    {language, medium}."""
    return "language", _DEFAULT_TIER


def resolve(modality: str, tier: str) -> ModelConfig:
    if modality != "language":
        raise NotImplementedError(
            f"modality {modality!r} is a registry placeholder only (docs/components/model-registry.md, "
            "\"Resolved: Registry Structure\") — no real dispatch logic exists for it yet"
        )
    if tier not in _LANGUAGE_TIERS:
        raise ValueError(f"unknown language tier {tier!r}, expected one of {_LANGUAGE_TIERS}")

    env_prefix = f"LANGUAGE_{tier.upper()}"
    model = os.environ.get(f"{env_prefix}_MODEL") or os.environ.get("PIONEER_MODEL", "")
    context_window = int(os.environ.get(f"{env_prefix}_CONTEXT_WINDOW", str(_DEFAULT_CONTEXT_WINDOW)))
    return ModelConfig(
        model=model,
        context_window=context_window,
        input_cost_per_token=_cost_per_token(env_prefix, "INPUT"),
        output_cost_per_token=_cost_per_token(env_prefix, "OUTPUT"),
        cached_input_cost_per_token=_cost_per_token(env_prefix, "CACHED_INPUT"),
    )


def _cost_per_token(env_prefix: str, kind: str) -> float:
    # Configured per-million-tokens (how providers publish pricing, e.g.
    # "$2.70 / 1M input tokens") rather than per-token directly — a raw
    # per-token float would mean typing "0.0000027" into a Helm value, easy
    # to mistype and hard to eyeball against a pricing page.
    raw = os.environ.get(f"{env_prefix}_{kind}_COST_PER_MILLION_TOKENS")
    if not raw:
        return 0.0
    return float(raw) / 1_000_000


def escalate(tier: str) -> str:
    """docs/components/model-registry.md, "Resolved: Escalate-on-Retry" —
    fast -> medium -> expert, capped at expert (never escalates past the
    top tier). An unrecognized tier escalates from the bottom, a safe
    default rather than raising mid-retry."""
    idx = _LANGUAGE_TIERS.index(tier) if tier in _LANGUAGE_TIERS else -1
    return _LANGUAGE_TIERS[min(idx + 1, len(_LANGUAGE_TIERS) - 1)]
