"""Provider cache keyed by (provider, base_url, api_key).

docs/components/model-registry.md, 2026-08-28 (third revision): every
language tier owns its own provider identity — provider kind + base URL
+ api key + max_tokens live on ModelConfig, not on a process-wide
default. This module owns the small amount of state that used to live
at worker startup — the concrete Provider instance itself — and returns
one per unique (provider, base_url, api_key) triple.

Why a cache rather than "just build one per call": each Provider wraps
an SDK client (AsyncOpenAI, AsyncAnthropic) which owns a real TCP
connection pool + TLS state. Constructing a fresh one per call defeats
connection reuse — every ModelCall would open a new HTTPS connection to
the provider, taking on the TLS handshake cost each time (~100-300ms
typically). Caching by the full triple keeps the previous single-client
behavior for the common case where every tier points at the same
provider (one entry in the cache, same pool as before), while genuinely
per-tier deployments transparently get one pool per distinct provider
without any caller having to think about it.

Why key by all three fields: (1) provider kind, because "openai" and
"anthropic" use genuinely different SDK types — they can't share a
cache entry even at the same URL; (2) base_url and api_key, because
they're set at SDK construction time (not per-request), so a
deployment pointing two tiers at the same base_url with different api
keys (say, one project's key for the cheap tier and another project's
key for the expert tier — distinct billing) needs two separate clients.

Not thread-safe intentionally: the tenant-worker runs on asyncio,
single event loop, so a plain dict is fine. If this ever moves to a
multi-threaded worker, wrap with an asyncio.Lock around the dict
mutation.
"""

from __future__ import annotations

import logging

from .model_registry import ModelConfig
from .providers import AnthropicProvider, OpenAIProvider, Provider

logger = logging.getLogger(__name__)

_providers: dict[tuple[str, str, str], Provider] = {}


def get_provider(config: ModelConfig) -> Provider:
    """Returns a cached Provider for this tier's config triple. Raises
    a clear error naming the missing field if the tier isn't fully
    configured — this is the per-tier equivalent of an early "no model
    resolved" check, done at the boundary this module owns."""
    if not config.provider:
        raise RuntimeError(
            "No provider resolved for this call — LANGUAGE_<TIER>_PROVIDER is unset "
            "for the tier this call resolved to. See docs/components/model-registry.md; "
            "every tier the deployment uses must set PROVIDER, MODEL, API_KEY, and "
            "(for provider=openai) BASE_URL."
        )
    if not config.api_key:
        raise RuntimeError(
            f"No provider api_key resolved for this call — LANGUAGE_<TIER>_API_KEY is "
            f"unset for the tier this call resolved to (provider={config.provider!r}). "
            f"See docs/components/model-registry.md."
        )
    if config.provider == "openai" and not config.base_url:
        raise RuntimeError(
            "No provider base_url resolved for this call — LANGUAGE_<TIER>_BASE_URL "
            "is unset for the tier this call resolved to (provider='openai' requires "
            "an explicit base_url — there's no single canonical endpoint the way "
            "provider='anthropic' has). See docs/components/model-registry.md."
        )

    key = (config.provider, config.base_url, config.api_key)
    provider = _providers.get(key)
    if provider is None:
        if config.provider == "openai":
            provider = OpenAIProvider(base_url=config.base_url, api_key=config.api_key)
        elif config.provider == "anthropic":
            provider = AnthropicProvider(base_url=config.base_url, api_key=config.api_key)
        else:
            # Should be unreachable — model_registry.resolve already
            # rejects unknown provider names — but named honestly rather
            # than an AttributeError somewhere deeper.
            raise RuntimeError(f"Unhandled provider kind {config.provider!r} in llm_client")
        _providers[key] = provider
        logger.info(
            "llm_client: constructed new %s Provider (base_url=%r)",
            config.provider, config.base_url or "<sdk default>",
        )
    return provider


async def close_all() -> None:
    """Shutdown hook — closes every cached provider. Called from
    tenant_worker.py's finally block."""
    for key, provider in list(_providers.items()):
        try:
            await provider.close()
        except Exception:  # noqa: BLE001 - best-effort cleanup on shutdown
            logger.warning("llm_client: error closing %s provider (base_url=%r)", key[0], key[1], exc_info=True)
    _providers.clear()
