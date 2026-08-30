// Package speech is the gateway's OpenAI-compatible speech I/O: batch and
// streaming transcription (STT), text-to-speech synthesis (TTS), and the
// deterministic transcript/response text helpers that sit around them
// (filler-word detection, "prepare this text to be spoken" sanitization).
//
// docs/components/gateway/discord-voice.md's "Resolved: STT/TTS as a
// Standard, Provider-Agnostic Contract". Used by both the live Discord
// voice pipeline (internal/gateway/discordvoice) and Discord text
// (internal/gateway/discord — voice-message transcription, spoken replies).
package speech

import "os"

// envOr is speech's own copy of the trivial env helper — kept local rather
// than importing another package for five lines, so this package depends on
// nothing but the standard library and its two client SDKs.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
