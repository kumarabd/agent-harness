"""Provider implementations for docs/components/model-registry.md's
multi-provider support (2026-08-28, third revision).

Each Provider owns one API-shape's worth of knowledge — how to translate
our internal OpenAI-shaped conversation format into that provider's
actual request, how to translate the response back, how to stream, how
to advertise tools. Callers use the Provider ABC and never touch
provider-specific SDKs directly.

Extending to a new-shape provider: add a class here implementing
Provider, then add a dispatch case in llm_client.get_provider. That's
the whole surface — no other file needs to know about the new provider.
"""

from .base import Provider, SimpleTextResult
from .openai_provider import OpenAIProvider
from .anthropic_provider import AnthropicProvider

__all__ = ["Provider", "SimpleTextResult", "OpenAIProvider", "AnthropicProvider"]
