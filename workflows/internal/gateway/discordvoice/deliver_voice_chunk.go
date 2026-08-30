package discordvoice

import (
	"context"
	"log"
	"time"

	"go.temporal.io/sdk/activity"

	"agent-harness/workflows/internal/gateway/speech"
)

// DeliverChunk synthesizes and plays one streamed sentence-chunk (docs/
// components/gateway.md's "Resolved: ModelCall Streaming", extended
// 2026-08-26 to Discord voice) — the voice equivalent of
// discordDeliverActivity.DeliverChunk (deliver_discord.go). Registered as
// "VoiceDeliverChunk" on the same embedded per-connection worker as Deliver
// (discord_voice.go's voiceJoin), since it needs the same live vc/bargeIn/
// lifecycle.
//
// turn_deliveries.content is CUMULATIVE (llm.py's call_model_streaming
// docstring — Discord's text edit needs the running total), but audio has
// no "edit": this reads this chunk's row AND the immediately preceding
// row's, and synthesizes only the DIFFERENCE — the new sentence(s) this
// chunk actually added — never the whole cumulative text again, or every
// chunk after the first would replay everything already spoken.
//
// Returns (interrupted, err) rather than a plain error, unlike
// DiscordDeliverChunk: turn.go's awaitModelCallWithStreaming needs to know
// when a barge-in has stopped this chunk's playback, so it can stop
// dispatching this turn's remaining chunks instead of talking over the
// human a second time — text has no equivalent concept.
func (a *voiceDeliverActivity) DeliverChunk(ctx context.Context, turnID string, seq int) (bool, error) {
	// Same read-check-then-claim-after-work idempotency pattern as Deliver
	// and DiscordDeliverChunk (both fixed 2026-08-26 for the same reason):
	// claiming `sent` before the real synthesis/playback would let a retry
	// after a genuine failure skip redoing it.
	var alreadyDelivered bool
	if err := a.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM turn_deliveries WHERE turn_id = $1 AND seq = $2 AND sent = true)",
		turnID, seq,
	).Scan(&alreadyDelivered); err != nil {
		return false, err
	}
	if alreadyDelivered {
		return false, nil
	}

	var cumulative string
	if err := a.pool.QueryRow(ctx,
		"SELECT content FROM turn_deliveries WHERE turn_id = $1 AND seq = $2", turnID, seq,
	).Scan(&cumulative); err != nil {
		return false, err
	}
	var previous string
	if err := a.pool.QueryRow(ctx,
		"SELECT COALESCE((SELECT content FROM turn_deliveries WHERE turn_id = $1 AND seq < $2 ORDER BY seq DESC LIMIT 1), '')",
		turnID, seq,
	).Scan(&previous); err != nil {
		return false, err
	}

	// content_buffer in llm.py's call_model_streaming only ever grows by
	// appending — every later chunk's cumulative content is guaranteed a
	// proper prefix extension of every earlier one, so this slice is always
	// safe (never negative-length, never a mismatched prefix).
	delta := ""
	if len(cumulative) > len(previous) {
		delta = cumulative[len(previous):]
	}
	// voice_text_sanitize.go's own comment — same deterministic emoji/
	// markdown backstop as Deliver's own, applied here per-chunk so a
	// sentence that's entirely (or partly) emoji is caught before the
	// delta == "" check below, not after.
	delta = speech.SanitizeForSpeech(delta)
	markSent := func() error {
		if _, err := a.pool.Exec(ctx,
			"UPDATE turn_deliveries SET sent = true WHERE turn_id = $1 AND seq = $2", turnID, seq,
		); err != nil {
			return err
		}
		_, err := a.pool.Exec(ctx, "UPDATE turns SET voice_streamed = true WHERE turn_id = $1", turnID)
		return err
	}
	if delta == "" {
		// Defensive only — llm.py's segmenter only calls on_chunk with new
		// content each time, so an empty delta shouldn't happen in
		// practice; nothing to synthesize either way.
		return false, markSent()
	}

	a.lifecycle.transitionTo(voiceLifecycleSynthesizing)
	ttsStart := time.Now()
	stream, err := speech.SynthesizePCM(ctx, delta)
	if err != nil {
		return false, err
	}
	defer stream.Close()
	// voice_tts_ttfb_seconds — same metric Deliver records, shared across
	// both delivery paths so a Grafana panel doesn't need to know which one
	// handled a given turn.
	activity.GetMetricsHandler(ctx).Timer("voice_tts_ttfb_seconds").Record(time.Since(ttsStart))

	enc, err := newVoiceOpusEncoder()
	if err != nil {
		return false, err
	}

	// Same bracket as Deliver's own — only one delivery (a chunk or the
	// whole-turn fallback) is ever in flight at a time for a given
	// connection, since turn.go's awaitModelCallWithStreaming dispatches
	// chunks strictly sequentially and the end-of-turn Deliver call only
	// starts after the whole reasoning loop (and thus every chunk) is done.
	stopChan := a.bargeIn.startPlayback()
	defer a.bargeIn.endPlayback(stopChan)
	a.lifecycle.transitionTo(voiceLifecyclePlaying)

	onFirstFrame := func() {
		metrics := activity.GetMetricsHandler(ctx)
		// Mutually exclusive in practice, not both-or-neither: seq 1 always
		// finds turnStart pending (chunkEnd was just zeroed by the same
		// markTurnStart call) and reports first-audio-latency; every later
		// chunk finds turnStart already consumed and reports the gap since
		// the previous chunk instead — docs/components/gateway/
		// discord-voice.md's Notes Log, the dead-air/stutter detector.
		if gap, ok := a.latency.takeSinceTurnStart(); ok {
			metrics.Timer("voice_first_audio_latency_seconds").Record(gap)
		}
		if gap, ok := a.latency.takeSinceLastChunkEnd(); ok {
			metrics.Timer("voice_chunk_gap_seconds").Record(gap)
		}
	}
	interrupted, err := streamPCMToOpus(ctx, stream, a.vc, enc, stopChan, onFirstFrame)
	a.latency.markChunkEnded()
	if err != nil {
		return false, err
	}
	if interrupted {
		// A resolved outcome, not a failure — same reasoning as Deliver's
		// own barge-in branch. Marked sent (not retried): replaying this
		// chunk's audio from the start on a hypothetical retry would be
		// wrong, the user already heard part of it and moved on, and
		// turn.go's own caller already stops dispatching this turn's
		// remaining chunks once it sees interrupted=true.
		a.lifecycle.transitionTo(voiceLifecycleInterrupted)
		return true, markSent()
	}
	if err := a.vc.Speaking(false); err != nil {
		// Cosmetic-only, same tolerance as Deliver's own final Speaking(false)
		// call — not worth turning into an activity error that would retry
		// (and re-synthesize/replay) this whole chunk over a speaking-state
		// flag failing to clear.
		log.Printf("discord-voice: failed to clear speaking state: %v", err)
	}
	return false, markSent()
}
