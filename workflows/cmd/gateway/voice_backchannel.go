package main

import "strings"

// voiceBackchannelWords — docs/components/gateway/discord-voice.md's "In
// Progress: Turn-Taking Model": the gap that model was explicitly flagged
// as NOT closing on its own. LiveKit's turn-detector v1-mini emits only an
// end-of-turn probability; the cloud-only v1 model's `backchannel_probability`
// output (confirmed directly from the real `languages.py` source: "the
// local mini model produces no backchannel probability") has no local
// equivalent. A real conversational category still needs handling: "yeah" /
// "okay" / "mhm" said WHILE the bot is talking, meaning "keep going, I'm
// following," not "stop, it's my turn" — the exact failure a real user
// report traced to (a turn where the user said "Yeah." and got back a
// completely empty assistant response).
//
// A DIFFERENT set from voiceFillerWords (voice_filler.go), deliberately —
// these are real, meaningful words in other contexts ("yes"/"okay" answer a
// real question) and must NOT be filtered unconditionally the way pure
// fillers are; isBackchannelOnly is only ever consulted when the utterance
// began while the bot was actively speaking (speakerBuffer's own
// startedDuringPlayback field, set at debounce-confirmation time) — the
// context that actually makes "yeah" a backchannel rather than an answer.
var voiceBackchannelWords = map[string]bool{
	"yeah": true, "yep": true, "yup": true, "ya": true,
	"okay": true, "ok": true, "kay": true,
	"right": true, "sure": true, "alright": true,
	"gotcha": true, "cool": true, "nice": true, "true": true,
	"mhm": true, "mmhm": true, "uh-huh": true, "uhhuh": true,
}

// isBackchannelOnly mirrors isFillerOnly's exact structure (voice_filler.go)
// — normalize, split, trim punctuation per word, reject on any word outside
// the set. Same strictness reasoning: a false positive here would silently
// drop a real message, so a single non-backchannel word anywhere in the
// transcript disqualifies the whole thing.
func isBackchannelOnly(text string) bool {
	words := strings.Fields(strings.ToLower(text))
	if len(words) == 0 {
		return false
	}
	sawWord := false
	for _, w := range words {
		w = strings.Trim(w, ".,!?;:\"'")
		if w == "" {
			continue
		}
		if !voiceBackchannelWords[w] {
			return false
		}
		sawWord = true
	}
	return sawWord
}
