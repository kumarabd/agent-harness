package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"os"
	"time"

	"github.com/bwmarrin/discordgo"
)

// docs/components/gateway/discord-voice.md's Notes Log — "filler injection"
// (a.k.a. acknowledgment phrases), the ChatGPT-voice-mode-style pattern
// discussed directly: play a short, PRE-synthesized phrase to mask
// voice_first_audio_latency_seconds' own dead air rather than reduce it —
// an explicit perceived-latency/UX technique, not a real optimization.
// Deliberately sequenced after the debounce fix and filler-transcript
// filtering (same discussion, same session) — building this on top of a
// VAD that still false-triggers on noise would just give a filler phrase
// that also gets falsely interrupted.
//
// Reactive, not immediate, since 2026-08-27 — the original "the instant an
// utterance is confirmed real" design caused the filler to collide with and
// get cut off by the real response once streaming made the common case fast
// enough for the two to race.
//
// Tiered, since 2026-08-29 (docs/components/gateway/discord-voice.md's Notes
// Log): a single filler said 1s after every request ("Let me check that."
// then 800ms later the answer) is its own kind of wrong — it makes a fast
// turn feel slower and more robotic. Instead there's a LADDER of phrases,
// each keyed to how long the wait has ALREADY lasted (voiceProgressPhrase.
// MinWait), climbed reactively: a genuinely fast answer preempts every tier
// before it speaks, a slow one gets a short ack and then, if it keeps going,
// a progress update. The gateway still can't PREDICT a turn's latency up
// front — tool calls, model-tier escalation, and context compaction are only
// known once ModelCall is already running — so the tier is chosen by how
// long the wait has ACTUALLY lasted, not by an expected latency.
//
// voiceProgressPhrases — pre-synthesized ONCE at gateway startup
// (synthesizeVoiceFillerCache, called from main.go), never per-turn: fresh
// synthesis would add its own TTS latency and defeat the entire point. Kept
// short and content-free — matching this file's own filler-word discipline,
// nothing here should ever look like it answered the actual question.
//
// MinWait is the elapsed-since-utterance-dispatched mark at which a tier
// becomes eligible to speak (if nothing is already playing). Ordered
// ascending. Each phrase itself speaks for ~1-1.5s, so the gaps here are
// deliberately WIDER than a raw "expected operation latency" bucketing
// (< 1s say nothing / 1-2s / 2-4s / 4-7s / > 7s) would suggest — adjacent
// tiers firing back-to-back sounds like a nervous person, not a patient
// assistant. Starting values, generous on purpose; tune against real usage,
// same category as voiceUtteranceSilenceFrames et al.
type voiceProgressPhrase struct {
	Text    string
	MinWait time.Duration
}

var voiceProgressPhrases = []voiceProgressPhrase{
	{Text: "One moment.", MinWait: 1 * time.Second},
	{Text: "Let me check that.", MinWait: 4 * time.Second},
	{Text: "Let me take a look.", MinWait: 8 * time.Second},
	{Text: "Still working on that.", MinWait: 14 * time.Second},
}

// fillerEnabled — opt-in, same convention as vadSidecarURL/whisperLiveURL:
// empty/unset means this deployment doesn't want it, and
// synthesizeVoiceFillerCache is never even attempted, so a tenant that
// hasn't opted in takes on no new startup dependency on kokoro-svc at all.
func fillerEnabled() bool {
	return os.Getenv("FILLER_ENABLED") == "true"
}

// voiceFillerCache holds each pre-synthesized progress phrase's raw PCM
// bytes (Kokoro's native wire format, unmodified — the same bytes
// synthesizeSpeechPCM returns, ready to feed straight into streamPCMToOpus
// via a bytes.Reader at play time). Built once at startup, shared read-only
// across every tenant connection afterward — the audio itself has no
// connection-specific content, so there's no reason to re-synthesize per
// connection; only voiceFillerPlayer's own playback trigger is per-
// connection. phrases is index-aligned to voiceProgressPhrases; an entry is
// nil if that one phrase failed pre-synthesis (best-effort, see below).
type voiceFillerCache struct {
	phrases [][]byte
}

// phraseAt returns tier i's PCM, or nil if that tier failed pre-synthesis
// (or i is out of range, or the cache is nil / filler disabled).
func (c *voiceFillerCache) phraseAt(i int) []byte {
	if c == nil || i < 0 || i >= len(c.phrases) {
		return nil
	}
	return c.phrases[i]
}

// hasAny reports whether at least one tier was successfully pre-synthesized —
// the cheap "is filler actually usable this run" check start does up front.
func (c *voiceFillerCache) hasAny() bool {
	if c == nil {
		return false
	}
	for _, p := range c.phrases {
		if p != nil {
			return true
		}
	}
	return false
}

// synthesizeVoiceFillerCache pre-synthesizes every voiceProgressPhrases
// entry via the same synthesizeSpeechPCM path real turns use. Best-effort,
// not fail-loud: this is an optional UX enhancement, not core to voice
// working at all (unlike Silero VAD's own fail-loud teardown) — a kokoro-svc
// hiccup at gateway startup degrades to "no filler this deployment" (or a
// partial ladder), not a failed boot. Called once from main.go, after other
// init; the returned cache is shared (read-only) across every connection
// this replica serves.
func synthesizeVoiceFillerCache(ctx context.Context) *voiceFillerCache {
	cache := &voiceFillerCache{phrases: make([][]byte, len(voiceProgressPhrases))}
	if !fillerEnabled() {
		return cache
	}
	synthesized := 0
	for i, tier := range voiceProgressPhrases {
		stream, err := synthesizeSpeechPCM(ctx, tier.Text)
		if err != nil {
			log.Printf("discord-voice: filler pre-synthesis failed for %q, skipping: %v", tier.Text, err)
			continue
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			log.Printf("discord-voice: filler pre-synthesis read failed for %q, skipping: %v", tier.Text, err)
			continue
		}
		cache.phrases[i] = data
		synthesized++
	}
	if synthesized == 0 {
		log.Printf("discord-voice: FILLER_ENABLED=true but no filler phrases could be pre-synthesized — filler injection is effectively disabled this run")
	} else {
		log.Printf("discord-voice: pre-synthesized %d/%d filler phrases", synthesized, len(voiceProgressPhrases))
	}
	return cache
}

// voiceFillerPlayer is connection-scoped — same one-instance-per-connection,
// constructed-in-voiceJoin treatment as voiceBargeIn/voiceLifecycle/
// voiceLatencyTracker — but holds no playback state of its own: a filler
// phrase is just another voiceBargeIn player, exactly like Deliver/
// DeliverChunk, and voiceBargeIn's own preemption (startPlayback/
// endPlayback's 2026-08-27 comments) is what actually stops a still-playing
// filler once the real response is ready — the moment Deliver/DeliverChunk
// makes their own already-existing startPlayback call, unmodified. This
// struct only exists to hold the shared cache reference; it's not
// coordinating anything itself.
type voiceFillerPlayer struct {
	cache *voiceFillerCache
}

func newVoiceFillerPlayer(cache *voiceFillerCache) *voiceFillerPlayer {
	return &voiceFillerPlayer{cache: cache}
}

// start climbs the voiceProgressPhrases ladder for one turn: for each tier,
// wait until its MinWait has elapsed since this call began, then — only if
// no real response (or prior-turn tail) has claimed playback in the
// meantime — speak that tier's phrase, and continue on to the next tier if
// the wait keeps running. Called from voiceCaptureLoop's flush closure, in
// its own goroutine (fire-and-forget: the caller has real turn-dispatch work
// of its own to get on with, not waiting on this). A no-op if filler
// injection isn't enabled for this deployment (cache has nothing to play).
//
// The reasoning behind reactive-and-tiered rather than one predicted phrase:
// nobody says "um, let me check" before answering an easy question
// instantly — a filled pause is a REACTIVE signal that thinking is taking a
// moment, and a SECOND one ("still working on it") is how a person signals
// the wait is running unusually long. The gateway can't predict which case
// a given turn is, but it can react to how long the wait actually turns out
// to be.
//
// Uses voiceBargeIn exactly like Deliver/DeliverChunk do — same
// startPlayback/endPlayback pairing, same streamPCMToOpus call. Once
// actually playing, a tier stops for either reason a real delivery would: a
// genuine user barge-in (signalSpeech), or being preempted by the real
// response's own startPlayback call once it's ready to play — voiceBargeIn's
// own preemption logic handles that automatically; this function doesn't
// know or care which one happened, same as Deliver/DeliverChunk never have.
func (f *voiceFillerPlayer) start(ctx context.Context, vc *discordgo.VoiceConnection, bargeIn *voiceBargeIn) {
	if !f.cache.hasAny() {
		return
	}

	startedAt := time.Now()
	for i, tier := range voiceProgressPhrases {
		phrase := f.cache.phraseAt(i)
		if phrase == nil {
			continue // this tier failed pre-synthesis; skip to the next
		}

		if wait := time.Until(startedAt.Add(tier.MinWait)); wait > 0 {
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
		}

		enc, err := newVoiceOpusEncoder()
		if err != nil {
			log.Printf("discord-voice: filler opus encoder failed: %v", err)
			return
		}

		stopChan, ok := bargeIn.tryStartPlayback()
		if !ok {
			// The real response (or, rarely, a prior turn's still-finishing
			// audio — bargeIn.isPlaying's own comment) already holds playback.
			// Saying anything now would overlap or immediately get cut off,
			// and no later tier would be right either — stop climbing. No log
			// line for this common case (would fire on every fast turn once
			// this feature is enabled).
			return
		}

		log.Printf("discord-voice: filler tier %d (%q) starting at %v elapsed", i, tier.Text, time.Since(startedAt).Round(time.Millisecond))
		played := time.Now()
		interrupted, streamErr := streamPCMToOpus(ctx, bytes.NewReader(phrase), vc, enc, stopChan, nil)
		bargeIn.endPlayback(stopChan)

		if streamErr != nil {
			log.Printf("discord-voice: filler playback failed: %v", streamErr)
			return
		}
		if interrupted {
			// Preempted by the real response's own startPlayback, or a
			// genuine user barge-in — streamPCMToOpus can't tell the two
			// apart and neither side needs it to. Either way this turn no
			// longer needs filler.
			log.Printf("discord-voice: filler tier %d cut off after %v (real response ready, or a genuine barge-in)", i, time.Since(played))
			return
		}
		log.Printf("discord-voice: filler tier %d finished naturally after %v", i, time.Since(played))
		// Loop on: if the wait runs past the next tier's MinWait, that
		// phrase plays as a progress update.
	}
}
