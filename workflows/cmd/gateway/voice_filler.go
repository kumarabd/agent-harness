package main

import "strings"

// voiceFillerWords — docs/components/gateway/discord-voice.md's Notes Log,
// the downstream follow-up to the debounce fix (2026-08-27): that fix stops
// a stray noise burst from ever producing a transcript in most cases, but a
// genuinely, clearly vocalized "um" can still get transcribed correctly by
// Whisper — this catches that case specifically.
//
// Deliberately a CONSERVATIVE, closed set of pure vocal fillers/backchannel
// sounds that carry no semantic content on their own. Never includes a real
// (if short) word like "yes"/"no"/"stop"/"hello" — those are legitimate
// one-word voice commands or responses, and filtering on word COUNT alone
// (rather than actual content) would silently drop them, a strictly worse
// regression than the noise problem this exists to fix. isFillerOnly only
// ever rejects a transcript that is ENTIRELY filler, never one containing
// even a single real word.
var voiceFillerWords = map[string]bool{
	"um": true, "umm": true, "ummm": true,
	"uh": true, "uhh": true, "uhm": true, "uhhh": true,
	"erm": true, "err": true,
	"hm": true, "hmm": true, "hmmm": true,
	"ah": true, "ahh": true,
	"huh": true,
	"mm":  true, "mhm": true, "mmhm": true,
}

// isFillerOnly reports whether text, once normalized (lowercased, split on
// whitespace, punctuation trimmed off each word), consists entirely of
// voiceFillerWords. Called right before a transcript would otherwise be
// dispatched as a real turn — a false positive here silently drops a real
// message, so this stays strict: any single word not in the set fails the
// whole check.
func isFillerOnly(text string) bool {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return false // empty text is handled separately by the caller's own text == "" check
	}
	sawWord := false
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:\"'")
		if w == "" {
			continue // pure punctuation token (e.g. a stray "..." on its own) — not itself a real word either way
		}
		if !voiceFillerWords[w] {
			return false
		}
		sawWord = true
	}
	// Guards the edge case where every "word" was pure punctuation (e.g.
	// text == "..." with nothing else) — not filler, just noise the
	// existing text == "" check upstream doesn't happen to catch since the
	// string itself isn't empty. Treated as non-filler (dispatched as-is)
	// rather than silently dropped — an unusual transcript like that is
	// this codebase's existing "don't paper over a real gap" territory, not
	// this function's problem to solve.
	return sawWord
}
