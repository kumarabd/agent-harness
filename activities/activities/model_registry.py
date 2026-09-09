"""Model Registry — docs/components/model-registry.md. Modality x Tier
structure (registry[modality][tier] -> model config). `language` is real,
three tiers (fast/medium/expert); vision/audio/video are placeholders only
— nothing in the current toolset calls them, so resolve() rejects them
rather than pretending to support dispatch that doesn't exist.

Storage: env vars, not Postgres — this is deployment-time configuration.

**Every tier owns its own provider identity, not just a model name**
(2026-08-28, second revision). Each configured language tier carries its
full provider quadruple — provider kind, API key, base URL, max_tokens —
alongside the model name and metadata, so different tiers can genuinely
point at different providers (fast at Groq, medium at DeepSeek, expert
at Anthropic, etc.) rather than sharing one process-wide provider
client. An earlier revision kept `LLM_PROVIDER_*` env vars as a shared
default under the tiers; that was removed in the same pass as the
model-name fallback, for the same reason — no silent shared state, every
configured tier declares what it needs explicitly.

**Real multi-provider abstraction landed 2026-08-28** (third revision,
same day): each tier declares its `provider` kind ("openai" |
"anthropic"), and a Provider ABC in activities/activities/providers/
owns the shape-conversion between our internal (OpenAI-shaped)
conversation format and each provider's actual API. The AsyncOpenAI /
AsyncAnthropic client under each Provider is constructed on demand and
cached per unique (provider, base_url, api_key) triple by llm_client.py,
so a deployment where all tiers happen to point at the same provider
still gets one shared HTTP connection pool for free.

"openai" as a provider name covers every OpenAI-API-compatible endpoint
— real OpenAI, DeepSeek, Qwen/DashScope, Groq, OpenRouter, Crusoe, etc.
— all of which speak the exact same protocol and differ only in base
URL + model catalog. "anthropic" is Anthropic's native Messages API
(system prompt separate; tool_use / tool_result content blocks; streaming
event shape differs from OpenAI's chunk shape). Extending to a
genuinely-new-shape provider is a new class under providers/
implementing the ABC + a dispatch case in llm_client — not a
half-generalized special case.

**No cross-tier fallback, no shared provider default.** Every language
tier the deployment actually uses must have LANGUAGE_<TIER>_PROVIDER,
LANGUAGE_<TIER>_MODEL, and LANGUAGE_<TIER>_API_KEY set (plus
LANGUAGE_<TIER>_BASE_URL for the "openai" provider — Anthropic's SDK
has its own default, so base_url is optional for provider="anthropic").
An unconfigured field surfaces at call time as a real error naming that
tier, not a silent fallback to another tier or to a shared default.
Consequence for deployments: `resolve()` returns a ModelConfig with
empty strings for anything unset, and each Provider's own checks turn
that into a real error at the point of use.
"""

from __future__ import annotations

import os
from dataclasses import dataclass

_LANGUAGE_TIERS = ("fast", "medium", "expert")
_DEFAULT_TIER = "medium"  # docs/components/model-registry.md, "Resolved: Selection Mechanism" — bootstrap default
_DEFAULT_CONTEXT_WINDOW = 128_000  # conservative fallback if a tier's own context window isn't configured
_DEFAULT_MAX_TOKENS = 4096  # request-tuning default; a tier can override via LANGUAGE_<TIER>_MAX_TOKENS

# Provider names a tier's LANGUAGE_<TIER>_PROVIDER may be set to. Extending
# this list requires a new class under activities/activities/providers/
# implementing the Provider ABC (see providers/base.py) and a dispatch
# case in llm_client.get_provider. Keep in sync with providers/__init__.py's
# own registry — that's where new providers register themselves.
_KNOWN_PROVIDERS = ("openai", "anthropic")


@dataclass(frozen=True)
class ModelConfig:
    model: str
    context_window: int
    # Per-tier provider identity — a tier owns its own PROVIDER, API key,
    # base URL, and max_tokens (2026-08-28), not a process-wide shared
    # default. Empty provider/api_key/base_url means unconfigured;
    # consumers (llm_client.get_provider, each Provider's own call site)
    # turn that into a real error at the point of use.
    #
    # provider: "openai" for any OpenAI-API-compatible endpoint (real
    # OpenAI, DeepSeek, Qwen/DashScope, Groq, OpenRouter, Crusoe, etc.),
    # "anthropic" for Anthropic's native Messages API. base_url is
    # required for openai (there's no single canonical endpoint), but
    # OPTIONAL for anthropic (the SDK has its own default) — pass an
    # empty string to accept the SDK's default.
    provider: str = ""
    api_key: str = ""
    base_url: str = ""
    max_tokens: int = _DEFAULT_MAX_TOKENS
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
    # No cross-tier fallback, no shared provider default — an unset
    # LANGUAGE_<TIER>_{PROVIDER,MODEL,API_KEY,BASE_URL} surfaces as an
    # empty string here, which llm_client.get_provider and each Provider
    # implementation turn into real errors naming this specific tier at
    # call time. See this module's own docstring for the reasoning.
    provider = os.environ.get(f"{env_prefix}_PROVIDER", "")
    if provider and provider not in _KNOWN_PROVIDERS:
        raise ValueError(
            f"unknown provider {provider!r} for tier {tier!r} — must be one of {_KNOWN_PROVIDERS}. "
            f"See docs/components/model-registry.md."
        )
    model = os.environ.get(f"{env_prefix}_MODEL", "")
    api_key = os.environ.get(f"{env_prefix}_API_KEY", "")
    base_url = os.environ.get(f"{env_prefix}_BASE_URL", "")
    context_window = int(os.environ.get(f"{env_prefix}_CONTEXT_WINDOW", str(_DEFAULT_CONTEXT_WINDOW)))
    max_tokens = int(os.environ.get(f"{env_prefix}_MAX_TOKENS", str(_DEFAULT_MAX_TOKENS)))
    return ModelConfig(
        model=model,
        context_window=context_window,
        provider=provider,
        api_key=api_key,
        base_url=base_url,
        max_tokens=max_tokens,
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
