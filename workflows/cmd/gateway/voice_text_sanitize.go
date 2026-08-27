package main

import (
	"regexp"
	"strings"
)

// sanitizeForSpeech strips characters with no natural spoken form before
// text reaches TTS (deliver_voice.go's Deliver, deliver_voice_chunk.go's
// DeliverChunk — never deliver_discord.go's text path, which needs markdown
// intact for Discord's own rendering). docs/components/gateway/
// discord-voice.md's Notes Log: a real, recurring complaint that survived
// platform_prompts.go's "never use emoji/markdown" instruction — a system
// prompt is a probabilistic instruction, not a guarantee, and Kokoro's own
// text normalization reads a stray emoji or markdown symbol out loud by
// name ("blue heart", "smiley face", "asterisk") rather than silently
// ignoring it. This is the deterministic backstop: applies regardless of
// what the model actually produced, the same defense-in-depth relationship
// voiceUtteranceMinSpeechFrames' debounce has with Silero VAD — a better
// upstream signal reduces how often the backstop has to act, but doesn't
// remove the need for one.
//
// Extended 2026-08-27 toward Vapi's own "Voice Formatting Plan" (a real
// production voice-AI platform's answer to this exact problem, confirmed
// "entirely rule-based, not ML-powered" — voice_text_normalize.go's own doc
// comment has the full research and the specific steps adopted vs.
// deliberately left out). Order matters: normalizeForSpeech's clock-time/
// phone/email/currency/percent patterns run FIRST, before markdown-symbol
// stripping below — a time's own colon or an email's own underscore must be
// recognized and consumed by those patterns before this function's more
// generic symbol-stripping would otherwise interfere with them.
func sanitizeForSpeech(text string) string {
	text = normalizeForSpeech(text)
	// A space, not empty string — real bug caught by this file's own
	// throwaway test before it shipped: deleting the symbol outright mashed
	// adjacent words together whenever the symbol was legitimately part of
	// a word rather than markdown decoration ("shell_exec" -> "shellexec").
	// A space plus the collapse pass below reads naturally either way:
	// "**bold**" -> "bold", "shell_exec" -> "shell exec".
	text = markdownSymbolPattern.ReplaceAllString(text, " ")
	text = stripEmoji(text)
	return collapseSpacesPattern.ReplaceAllString(strings.TrimSpace(text), " ")
}

var (
	markdownSymbolPattern = regexp.MustCompile("[*_`~#]")
	collapseSpacesPattern = regexp.MustCompile(`[ \t]{2,}`)
)

// stripEmoji removes runes drawn from the Unicode ranges emoji actually come
// from, plus the modifiers that turn a base character into a compound emoji
// (variation selector, zero-width joiner, skin-tone modifiers, regional
// indicators for flags). Verified against the exact characters in the real
// report this fixes: "blue heart" is U+1F499 (in 1F300-1FAFF), "smiley
// face" territory is U+1F600-1F64F (also in 1F300-1FAFF).
//
// Not a full emoji database (Unicode's own emoji-data.txt has finer-grained
// exceptions within these blocks) — a range-based filter is deliberately
// approximate, matching this function's own "narrow, not a parser" scope.
func stripEmoji(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range text {
		if isEmojiRune(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF: // misc symbols/pictographs, emoticons, transport, supplemental symbols
		return true
	case r >= 0x2600 && r <= 0x27BF: // misc symbols, dingbats
		return true
	case r >= 0x2B00 && r <= 0x2BFF: // misc symbols and arrows (e.g. the star, U+2B50)
		return true
	case r >= 0x2300 && r <= 0x23FF: // misc technical (e.g. watch/alarm-clock glyphs commonly used as emoji)
		return true
	case r >= 0x1F1E6 && r <= 0x1F1FF: // regional indicators (flag pairs)
		return true
	case r == 0xFE0F: // variation selector-16 (forces emoji presentation)
		return true
	case r == 0x200D: // zero-width joiner (compound emoji)
		return true
	case r >= 0x1F3FB && r <= 0x1F3FF: // skin tone modifiers
		return true
	}
	return false
}
