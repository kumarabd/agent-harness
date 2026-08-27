package main

import (
	"bytes"
	"context"
	"io"
	"log"
	"math/rand"
	"os"

	"github.com/bwmarrin/discordgo"
)

// docs/components/gateway/discord-voice.md's Notes Log — "filler injection"
// (a.k.a. acknowledgment phrases), the ChatGPT-voice-mode-style pattern
// discussed directly: play a short, PRE-synthesized phrase the instant an
// utterance is confirmed real, to mask voice_first_audio_latency_seconds'
// own dead air rather than reduce it — an explicit perceived-latency/UX
// technique, not a real optimization. Deliberately sequenced after the
// debounce fix and filler-transcript filtering (same discussion, same
// session) — building this on top of a VAD that still false-triggers on
// noise would just give a filler phrase that also gets falsely interrupted.
//
// voiceFillerPhraseText — pre-synthesized ONCE at gateway startup
// (synthesizeVoiceFillerCache, called from main.go), never per-turn: fresh
// synthesis would add its own TTS latency and defeat the entire point.
// Rotated randomly per play so it doesn't sound identical every turn. Kept
// short and content-free — matching this file's own filler-word discipline,
// nothing here should ever look like it answered the actual question.
var voiceFillerPhraseText = []string{
	"Let me check that.",
	"One moment.",
	"Let's see.",
	"Give me a second.",
	"Hmm, one moment.",
}

// fillerEnabled — opt-in, same convention as vadSidecarURL/whisperLiveURL:
// empty/unset means this deployment doesn't want it, and
// synthesizeVoiceFillerCache is never even attempted, so a tenant that
// hasn't opted in takes on no new startup dependency on kokoro-svc at all.
func fillerEnabled() bool {
	return os.Getenv("FILLER_ENABLED") == "true"
}

// voiceFillerCache holds every pre-synthesized filler phrase's raw PCM
// bytes (Kokoro's native wire format, unmodified — the same bytes
// synthesizeSpeechPCM returns, ready to feed straight into streamPCMToOpus
// via a bytes.Reader at play time). Built once at startup, shared read-only
// across every tenant connection afterward — the audio itself has no
// connection-specific content, so there's no reason to re-synthesize per
// connection; only voiceFillerPlayer's own playback trigger is per-
// connection.
type voiceFillerCache struct {
	phrases [][]byte
}

func (c *voiceFillerCache) randomPhrase() []byte {
	if c == nil || len(c.phrases) == 0 {
		return nil
	}
	return c.phrases[rand.Intn(len(c.phrases))]
}

// synthesizeVoiceFillerCache pre-synthesizes every voiceFillerPhraseText
// entry via the same synthesizeSpeechPCM path real turns use. Best-effort,
// not fail-loud: this is an optional UX enhancement, not core to voice
// working at all (unlike Silero VAD's own fail-loud teardown) — a kokoro-svc
// hiccup at gateway startup degrades to "no filler this deployment", not a
// failed boot. Called once from main.go, after other init; the returned
// cache is shared (read-only) across every connection this replica serves.
func synthesizeVoiceFillerCache(ctx context.Context) *voiceFillerCache {
	cache := &voiceFillerCache{}
	if !fillerEnabled() {
		return cache
	}
	for _, phrase := range voiceFillerPhraseText {
		stream, err := synthesizeSpeechPCM(ctx, phrase)
		if err != nil {
			log.Printf("discord-voice: filler pre-synthesis failed for %q, skipping: %v", phrase, err)
			continue
		}
		data, err := io.ReadAll(stream)
		stream.Close()
		if err != nil {
			log.Printf("discord-voice: filler pre-synthesis read failed for %q, skipping: %v", phrase, err)
			continue
		}
		cache.phrases = append(cache.phrases, data)
	}
	if len(cache.phrases) == 0 {
		log.Printf("discord-voice: FILLER_ENABLED=true but no filler phrases could be pre-synthesized — filler injection is effectively disabled this run")
	} else {
		log.Printf("discord-voice: pre-synthesized %d/%d filler phrases", len(cache.phrases), len(voiceFillerPhraseText))
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

// start plays one random cached filler phrase — called from
// voiceCaptureLoop's flush closure, in its own goroutine (fire-and-forget:
// the caller has real turn-dispatch work of its own to get on with, not
// waiting on a filler phrase to finish speaking). A no-op if filler
// injection isn't enabled for this deployment (cache has nothing to play).
//
// Uses voiceBargeIn exactly like Deliver/DeliverChunk do — same
// startPlayback/endPlayback pairing, same streamPCMToOpus call. Stops for
// either reason a real delivery would: a genuine user barge-in
// (signalSpeech), or being preempted by the real response's own
// startPlayback call once it's ready to play — voiceBargeIn's own
// preemption logic handles that automatically; this function doesn't know
// or care which one happened, same as Deliver/DeliverChunk never have.
func (f *voiceFillerPlayer) start(ctx context.Context, vc *discordgo.VoiceConnection, bargeIn *voiceBargeIn) {
	phrase := f.cache.randomPhrase()
	if phrase == nil {
		return
	}

	enc, err := newVoiceOpusEncoder()
	if err != nil {
		log.Printf("discord-voice: filler opus encoder failed: %v", err)
		return
	}

	stopChan := bargeIn.startPlayback()
	defer bargeIn.endPlayback(stopChan)
	if _, err := streamPCMToOpus(ctx, bytes.NewReader(phrase), vc, enc, stopChan, nil); err != nil {
		log.Printf("discord-voice: filler playback failed: %v", err)
	}
}
