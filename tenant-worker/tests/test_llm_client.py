"""llm_client.py — the provider cache and its misconfiguration errors.

Constructing a provider opens no connection, so the whole module is reachable here.
The errors are the point: "no provider resolved" is the per-tier equivalent of an
early "no model" check, and the message has to name the tier or the operator is left
grepping a Helm values file.
"""

import pytest

from tenant_worker import llm_client
from tenant_worker.model_registry import ModelConfig
from tenant_worker.providers import AnthropicProvider, OpenAIProvider


def make_config(provider="openai", api_key="sk-x", base_url="https://api.example/v1", model="m"):
    return ModelConfig(
        model=model,
        context_window=1000,
        provider=provider,
        api_key=api_key,
        base_url=base_url,
        max_tokens=100,
        input_cost_per_token=0.0,
        output_cost_per_token=0.0,
        cached_input_cost_per_token=0.0,
    )


@pytest.fixture(autouse=True)
def empty_cache(monkeypatch):
    """The cache is module-level, so a test that constructs a provider would
    otherwise leak it into the next one — and these tests are about what gets
    constructed."""
    monkeypatch.setattr(llm_client, "_providers", {})


def test_an_unset_provider_names_the_variable_and_the_required_set():
    with pytest.raises(RuntimeError, match="LANGUAGE_<TIER>_PROVIDER is unset"):
        llm_client.get_provider(make_config(provider=""))


def test_an_unset_api_key_is_reported_against_the_provider_that_was_resolved():
    with pytest.raises(RuntimeError, match="api_key"):
        llm_client.get_provider(make_config(api_key=""))


def test_openai_demands_an_explicit_base_url():
    # Unlike anthropic, openai-compatible endpoints have no canonical host — the
    # same config string points at DeepSeek, Groq or Crusoe depending on the deploy.
    with pytest.raises(RuntimeError, match="base_url"):
        llm_client.get_provider(make_config(provider="openai", base_url=""))


def test_anthropic_is_allowed_to_accept_the_sdks_own_default_endpoint():
    provider = llm_client.get_provider(make_config(provider="anthropic", base_url=""))
    assert isinstance(provider, AnthropicProvider)


def test_providers_are_cached_by_their_config_triple():
    first = llm_client.get_provider(make_config())
    assert llm_client.get_provider(make_config()) is first

    # A different key is a different provider, not the same one reused.
    other = llm_client.get_provider(make_config(api_key="sk-y"))
    assert other is not first and isinstance(other, OpenAIProvider)


def test_an_unhandled_provider_kind_is_named_rather_than_crashing_deeper():
    # model_registry.resolve rejects unknown names first, so this is unreachable in
    # practice — but an AttributeError from inside a constructor is a much worse
    # thing to debug than a sentence saying what happened.
    with pytest.raises(RuntimeError, match="Unhandled provider kind"):
        llm_client.get_provider(make_config(provider="mystery"))
