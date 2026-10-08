"""model_registry.py — which model a tier resolves to, and how it escalates.

`resolve` reads the environment at call time and is the only place a tier becomes a
provider, model, key and budget. It is also where a typo in a Helm value turns into a
turn that cannot run, so the error paths matter as much as the happy one.
"""

import pytest

from tenant_worker import model_registry as registry


def test_the_first_step_of_a_turn_defaults_to_language_medium():
    assert registry.default_hint() == ("language", "medium")


def test_resolving_a_tier_reads_that_tiers_own_environment(monkeypatch):
    monkeypatch.setenv("LANGUAGE_FAST_PROVIDER", "openai")
    monkeypatch.setenv("LANGUAGE_FAST_MODEL", "gpt-4o-mini")
    monkeypatch.setenv("LANGUAGE_FAST_API_KEY", "sk-x")
    monkeypatch.setenv("LANGUAGE_FAST_BASE_URL", "https://api.example/v1")
    monkeypatch.setenv("LANGUAGE_FAST_MAX_TOKENS", "1234")

    config = registry.resolve("language", "fast")

    # Per-tier, not process-wide: a tier owns its own provider, key and budget.
    assert (config.provider, config.model, config.api_key) == ("openai", "gpt-4o-mini", "sk-x")
    assert config.base_url == "https://api.example/v1"
    assert config.max_tokens == 1234


def test_an_unconfigured_tier_resolves_to_empty_strings_rather_than_guessing(monkeypatch):
    for name in ("PROVIDER", "MODEL", "API_KEY", "BASE_URL"):
        monkeypatch.delenv(f"LANGUAGE_EXPERT_{name}", raising=False)

    config = registry.resolve("language", "expert")

    # Empty is the documented "unconfigured" signal; llm_client turns it into an
    # error naming this specific tier. A cross-tier fallback here would silently
    # run the wrong model instead.
    assert (config.provider, config.model, config.api_key, config.base_url) == ("", "", "", "")
    assert config.context_window == registry._DEFAULT_CONTEXT_WINDOW
    assert config.max_tokens == registry._DEFAULT_MAX_TOKENS


def test_an_unknown_tier_is_rejected():
    with pytest.raises(ValueError, match="unknown language tier"):
        registry.resolve("language", "turbo")


def test_an_unknown_provider_is_rejected_with_the_known_ones_named(monkeypatch):
    monkeypatch.setenv("LANGUAGE_FAST_PROVIDER", "gemini")
    with pytest.raises(ValueError, match="unknown provider"):
        registry.resolve("language", "fast")


def test_modalities_other_than_language_are_declared_placeholders():
    # Named honestly rather than failing deeper with an AttributeError.
    with pytest.raises(NotImplementedError, match="placeholder"):
        registry.resolve("vision", "fast")


def test_costs_are_configured_per_million_and_stored_per_token(monkeypatch):
    monkeypatch.setenv("LANGUAGE_FAST_INPUT_COST_PER_MILLION_TOKENS", "2.50")
    monkeypatch.setenv("LANGUAGE_FAST_OUTPUT_COST_PER_MILLION_TOKENS", "10")
    config = registry.resolve("language", "fast")
    # Configured the way providers publish pricing, so a Helm value can be eyeballed
    # against a pricing page.
    assert config.input_cost_per_token == pytest.approx(0.0000025)
    assert config.output_cost_per_token == pytest.approx(0.00001)


def test_an_unset_cost_is_zero_rather_than_an_error(monkeypatch):
    monkeypatch.delenv("LANGUAGE_FAST_INPUT_COST_PER_MILLION_TOKENS", raising=False)
    assert registry.resolve("language", "fast").input_cost_per_token == 0.0


@pytest.mark.parametrize(
    "tier,expected",
    [("fast", "medium"), ("medium", "expert"), ("expert", "expert")],
)
def test_escalation_walks_up_and_stops_at_the_top(tier, expected):
    assert registry.escalate(tier) == expected


def test_an_unrecognized_tier_escalates_from_the_bottom():
    # A safe default rather than raising mid-retry, when a turn is already in trouble.
    assert registry.escalate("mystery") == "fast"
