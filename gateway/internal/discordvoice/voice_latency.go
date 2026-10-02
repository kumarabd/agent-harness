package discordvoice

import (
	"sync"
	"time"
)

// voiceLatencyTracker is Gateway-local, in-memory, connection-scoped state
// for real voice-latency numbers (docs/components/gateway/discord-voice.md's
// Notes Log — "get a real TTFB number so we're not guessing" was the
// explicit ask this exists to answer) — same Media Plane, one-instance-
// per-connection, constructed-once-in-voiceJoin treatment as voiceBargeIn
// and voiceLifecycle, which this shares construction/passing with.
//
// Correlating "when did the user stop talking" (voiceCaptureLoop, capture
// side) with "when did the bot's audio actually start" (voiceDeliverActivity,
// delivery side) would normally need a real trace ID threaded across the
// Temporal boundary between them. It doesn't need one here: both sides
// live in the SAME process for a given connection, for as long as this
// replica holds the connection's lease — connection-scoped, not per-turn,
// mirrors the same simplification voiceLifecycle's own comment already
// accepted for the same reason (nothing here needs per-turn identity to be
// useful for a latency signal, only "how long since the thing that should
// have triggered this").
type voiceLatencyTracker struct {
	mu sync.Mutex
	// turnStart — set when an utterance's transcription completes and a
	// MessageEvent is about to be submitted; zero when nothing is pending.
	// One-shot: takeSinceTurnStart clears it, so only the genuinely FIRST
	// audio frame sent in response consumes it, regardless of whether that
	// happens inside a streamed chunk or the whole-turn fallback Deliver.
	turnStart time.Time
	// chunkEnd — when the most recently sent chunk's (or the last turn's
	// non-streamed) audio finished sending; zero when there's no prior
	// chunk to gap against (the very first chunk of a turn, or nothing sent
	// yet at all).
	chunkEnd time.Time
}

func newVoiceLatencyTracker() *voiceLatencyTracker {
	return &voiceLatencyTracker{}
}

// markTurnStart records "an utterance just finished, a new turn is about to
// be submitted" — called from voiceCaptureLoop right before
// core.Ingest. Also clears chunkEnd: a stale gap from a PREVIOUS,
// unrelated turn must never be reported against this new one's first chunk.
func (t *voiceLatencyTracker) markTurnStart() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.turnStart = time.Now()
	t.chunkEnd = time.Time{}
}

// takeSinceTurnStart returns the elapsed time since the last markTurnStart
// call and clears it, or ok=false if nothing is pending — e.g. a retried
// delivery activity re-running after a genuine failure (the real first
// frame already consumed this on the earlier attempt), or this connection's
// process having restarted mid-turn. Not calling this a failure: a missed
// sample just means this one turn doesn't contribute to the histogram,
// nothing more.
func (t *voiceLatencyTracker) takeSinceTurnStart() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.turnStart.IsZero() {
		return 0, false
	}
	elapsed := time.Since(t.turnStart)
	t.turnStart = time.Time{}
	return elapsed, true
}

// takeSinceLastChunkEnd returns the gap since the previous chunk's audio
// finished sending, or ok=false if there was no previous chunk (the first
// chunk of a turn). Also one-shot in spirit — markChunkEnded overwrites
// chunkEnd on every send, so this always measures the gap against
// whichever chunk most recently finished, never a stale one from further
// back.
func (t *voiceLatencyTracker) takeSinceLastChunkEnd() (time.Duration, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.chunkEnd.IsZero() {
		return 0, false
	}
	return time.Since(t.chunkEnd), true
}

// markChunkEnded records "a chunk's (or the whole turn's) audio just
// finished sending" — called at the end of every streamPCMToOpus call,
// success or barge-in-interrupted alike (either way, this connection just
// stopped producing audio at this instant, which is exactly what the next
// chunk's gap should be measured against).
func (t *voiceLatencyTracker) markChunkEnded() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunkEnd = time.Now()
}
