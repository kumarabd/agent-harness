package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"layeh.com/gopus"
)

// voiceDeliverActivity is docs/components/gateway/discord-voice.md's
// "Resolved: Outbound — TTS + Opus Playback, Not ChannelMessageSend" —
// structurally the same shape as discordDeliverActivity (deliver_discord.go),
// registered as "VoiceDeliver" on this voice connection's own embedded
// Temporal worker (discord_voice.go's voiceJoin), for exactly as long as
// this replica holds the connection's lease.
type voiceDeliverActivity struct {
	vc           *discordgo.VoiceConnection
	pool         *pgxpool.Pool
	connectionID string
	// bargeIn — docs/components/gateway.md's "Resolved: Voice Platforms —
	// Cascaded Architecture" fast-path barge-in. Shared with
	// discord_voice.go's voiceCaptureLoop for this same connection
	// (constructed once in voiceJoin, passed to both), same required-field
	// treatment as vc/pool above.
	bargeIn *voiceBargeIn
	// lifecycle — docs/components/gateway/discord-voice.md's "Resolved:
	// Turn Lifecycle Stays Gateway-Local". Same connection, same
	// construct-once-in-voiceJoin, shared-with-voiceCaptureLoop treatment
	// as bargeIn above.
	lifecycle *voiceLifecycle
	// latency — docs/components/gateway/discord-voice.md's Notes Log, real
	// voice-latency metrics. Same one-instance-per-connection,
	// shared-with-voiceCaptureLoop treatment as bargeIn/lifecycle above.
	latency *voiceLatencyTracker
}

// Deliver reads the turn's final assistant message, checks-then-inserts
// into delivered_responses (unchanged idempotency ledger — response_id =
// turn_id, same as text), then synthesizes and streams it to vc.OpusSend
// incrementally — Opus-encoding and sending each frame as its bytes arrive
// off Kokoro's own chunked HTTP response, rather than buffering the whole
// synthesized response before sending anything (docs/components/gateway.md's
// "Resolved: Voice Platforms — Cascaded Architecture," the pipelining
// principle: start playing "Sure, I can" while more is still being
// synthesized, not after all of it is). discordgo's own opusSender paces
// the actual send rate and signals speaking-state automatically; this only
// needs to hand it frames as they're ready.
//
// No channel_id lookup needed here the way DiscordDeliver needs one: this
// activity is constructed fresh per voice connection, already bound to the
// one channel it can ever deliver to (vc itself), unlike DiscordDeliver's
// single worker serving every channel a bot's text connection touches.
func (a *voiceDeliverActivity) Deliver(ctx context.Context, turnID string) error {
	// Real, live bug fixed 2026-08-26: this used to INSERT the idempotency
	// row here, BEFORE any real work — so a genuine failure partway through
	// (confirmed live: kokoro-svc OOM-killed mid-synthesis) still left the
	// row committed, and Temporal's own automatic retry (attempt 2) saw
	// "already delivered" and returned success in ~4ms without ever
	// retrying the actual synthesis/delivery — the turn silently produced
	// no audio at all. A read-only check here, with the real INSERT moved
	// to every genuine-completion return point below, means a retry after
	// a real failure correctly finds no row and redoes the real work,
	// while a retry after real success (Temporal's own rare at-least-once
	// double-dispatch case) still correctly finds the row and skips it.
	var alreadyDelivered bool
	if err := a.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM delivered_responses WHERE response_id = $1)", turnID,
	).Scan(&alreadyDelivered); err != nil {
		return err
	}
	if alreadyDelivered {
		return nil
	}
	markDelivered := func() error {
		_, err := a.pool.Exec(ctx,
			"INSERT INTO delivered_responses (response_id) VALUES ($1) ON CONFLICT DO NOTHING", turnID)
		return err
	}

	// Every real exit path below (success, error, ctx cancel, barge-in) is
	// meant to leave the connection back at "listening" — the resting
	// state between turns — so this is the one place that needs to cover
	// all of them, rather than repeating a transitionTo at each return.
	defer a.lifecycle.transitionTo(voiceLifecycleListening)

	var content string
	err := a.pool.QueryRow(ctx,
		"SELECT content FROM messages WHERE parent_id = $1 AND role = 'assistant' ORDER BY seq DESC LIMIT 1",
		turnID,
	).Scan(&content)
	if err != nil {
		return err
	}

	// 007_voice_streaming_delivery.sql's own comment: mirrors DiscordDeliver's
	// streamed_message_ref check. Real, live bug fixed 2026-08-27 (same root
	// cause and same fix shape as DiscordDeliver's own): voice_streamed only
	// ever means "this turn's first iteration was streamed" (turn.go's
	// streamingEligible gate is iterations==1, gated purely on iteration
	// count, not on whether the turn stopped there) — true regardless of
	// whether that first iteration made a tool call and the turn went on to
	// produce a LATER, never-streamed final answer from a later iteration.
	// The old code treated voiceStreamed as "the whole turn was already
	// spoken" and skipped unconditionally — confirmed live: a real turn
	// that called a tool completed with the correct final answer sitting in
	// `messages`, and this activity kept reporting success having spoken
	// nothing but the streamed "let me check" remark, forever. Comparing
	// against turn_deliveries' own last cumulative row (what was actually
	// spoken) tells the two cases apart: equal means iteration 1 WAS the
	// whole turn and DeliverChunk's last chunk already spoke this exact
	// content — skip, don't repeat it. Different means this is a genuinely
	// later, unspoken message — fall through below and actually speak it,
	// same as a turn that was never streamed at all.
	var voiceStreamed bool
	if err := a.pool.QueryRow(ctx,
		"SELECT voice_streamed FROM turns WHERE turn_id = $1", turnID,
	).Scan(&voiceStreamed); err != nil {
		return err
	}
	if voiceStreamed {
		var streamedContent string
		if err := a.pool.QueryRow(ctx,
			"SELECT COALESCE((SELECT content FROM turn_deliveries WHERE turn_id = $1 ORDER BY seq DESC LIMIT 1), '')",
			turnID,
		).Scan(&streamedContent); err != nil {
			return err
		}
		if streamedContent == content {
			return markDelivered()
		}
	}
	// voice_text_sanitize.go's own comment — a deterministic backstop for
	// emoji/markdown that platform_prompts.go's system prompt instruction
	// doesn't reliably prevent on its own. Applied before the empty check
	// below so a message that was ENTIRELY emoji (content != "" but
	// sanitizes to "") correctly takes the same "nothing to synthesize"
	// path, not an empty-string TTS call.
	content = sanitizeForSpeech(content)
	if content == "" {
		// docs/future-work.md §4 — a real, separately-tracked gap (the
		// model sometimes ends a turn with no real content). Nothing to
		// synthesize; not this activity's job to paper over it. Still a
		// genuine resolution, not a failure — mark delivered so a rare
		// duplicate dispatch doesn't re-run this check pointlessly.
		return markDelivered()
	}

	a.lifecycle.transitionTo(voiceLifecycleSynthesizing)
	ttsStart := time.Now()
	stream, err := synthesizeSpeechPCM(ctx, content)
	if err != nil {
		return err
	}
	defer stream.Close()
	// voice_tts_ttfb_seconds — time to Kokoro's response headers, i.e.
	// before any of its body has necessarily been read yet: isolates TTS
	// service latency specifically, separate from voice_first_audio_latency_seconds
	// below (which also includes this activity's own DB reads and the
	// Opus-encode of the first frame).
	activity.GetMetricsHandler(ctx).Timer("voice_tts_ttfb_seconds").Record(time.Since(ttsStart))

	enc, err := newVoiceOpusEncoder()
	if err != nil {
		return err
	}

	// Fast-path barge-in window: brackets only the actual audio-sending
	// loop below, not the DB reads/TTS request above it — nothing is
	// playing yet during synthesis, so there's nothing for a barge-in
	// signal to usefully stop before this point.
	stopChan := a.bargeIn.startPlayback()
	defer a.bargeIn.endPlayback(stopChan)
	a.lifecycle.transitionTo(voiceLifecyclePlaying)

	onFirstFrame := func() {
		// docs/components/gateway/discord-voice.md's Notes Log — the
		// headline "how long did they wait before hearing anything" number.
		// One-shot via voiceLatencyTracker: only fires for whichever call
		// (this whole-turn Deliver, or one of DeliverChunk's chunks)
		// genuinely produces the turn's first audio.
		if gap, ok := a.latency.takeSinceTurnStart(); ok {
			activity.GetMetricsHandler(ctx).Timer("voice_first_audio_latency_seconds").Record(gap)
		}
	}
	interrupted, err := streamPCMToOpus(ctx, stream, a.vc, enc, stopChan, onFirstFrame)
	a.latency.markChunkEnded()
	if err != nil {
		return err
	}
	if interrupted {
		// docs/components/gateway.md's "Resolved: Voice Platforms —
		// Cascaded Architecture" fast-path barge-in: VAD detected speech
		// while this connection was actively playing. Deliberately treated
		// as a resolved outcome, not a failure — stopping mid-playback
		// because a human started talking is correct here. The slow path
		// (turn.go's interrupt-race) separately decides what to do with
		// whatever the human actually said, once it's transcribed — this
		// only ever stops local audio output.
		a.lifecycle.transitionTo(voiceLifecycleInterrupted)
		log.Printf("discord-voice: playback interrupted by barge-in for turn %s", turnID)
		// A resolved outcome, not a failure — replaying from the start on a
		// hypothetical retry would be wrong anyway (the user already heard
		// part of it and moved on).
		return markDelivered()
	}
	if err := a.vc.Speaking(false); err != nil {
		log.Printf("discord-voice: failed to clear speaking state: %v", err)
	}
	log.Printf("discord-voice: delivered turn %s via connection %s", turnID, a.connectionID)
	return markDelivered()
}

// DeliverInterim speaks a pending user_input_requests row's prompt+options
// — docs/components/user-input.md's "Mid-turn interim delivery" (push half,
// A+B). Takes requestID, not turnID: everything this needs (prompt, options,
// and its own idempotency marker) lives on that row itself, written by
// RequestUserInput right before UserInputRequestWorkflow dispatches this —
// the same reference-passing shape Deliver's own turnID param has, just
// pointed at a different table.
//
// Deliberately the simplest possible spoken rendering — prompt, then each
// option's label read out with its number — not a real dialogue turn (no
// attempt at natural phrasing/TTS-friendly restructuring the way
// platform_prompts.go's system prompt shapes a real model response).
// Response-routing (recognizing the next utterance as an answer to THIS
// request rather than a new ordinary turn) is real, separate, harder future
// scope — gateway/discord-voice.md doesn't have a button equivalent for
// voice — not attempted here; this only closes the push half.
//
// Idempotency via prompt_delivered_at, independent of delivered_responses
// (that ledger is turn-final-answer-only) and independent of
// user_input_requests.status — same reasoning as DiscordDeliverInterim's
// own comment (deliver_discord.go).
func (a *voiceDeliverActivity) DeliverInterim(ctx context.Context, requestID string) error {
	var alreadyDelivered bool
	if err := a.pool.QueryRow(ctx,
		"SELECT prompt_delivered_at IS NOT NULL FROM user_input_requests WHERE request_id = $1", requestID,
	).Scan(&alreadyDelivered); err != nil {
		return err
	}
	if alreadyDelivered {
		return nil
	}

	var prompt string
	var optionsJSON []byte
	if err := a.pool.QueryRow(ctx,
		"SELECT prompt, options FROM user_input_requests WHERE request_id = $1", requestID,
	).Scan(&prompt, &optionsJSON); err != nil {
		return err
	}
	var options []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(optionsJSON, &options); err != nil {
		return err
	}

	spoken := prompt
	for i, opt := range options {
		spoken += fmt.Sprintf(". Option %d: %s", i+1, opt.Label)
	}
	spoken = sanitizeForSpeech(spoken)
	if spoken == "" {
		// Nothing left to speak (an entirely-emoji prompt would be unusual,
		// but same defensive posture as Deliver's own empty-content check) —
		// still a resolved outcome, mark delivered rather than leave the
		// idempotency check finding nothing forever.
		_, err := a.pool.Exec(ctx, "UPDATE user_input_requests SET prompt_delivered_at = now() WHERE request_id = $1", requestID)
		return err
	}

	stream, err := synthesizeSpeechPCM(ctx, spoken)
	if err != nil {
		return err
	}
	defer stream.Close()

	enc, err := newVoiceOpusEncoder()
	if err != nil {
		return err
	}

	stopChan := a.bargeIn.startPlayback()
	defer a.bargeIn.endPlayback(stopChan)
	a.lifecycle.transitionTo(voiceLifecyclePlaying)
	defer a.lifecycle.transitionTo(voiceLifecycleListening)

	interrupted, err := streamPCMToOpus(ctx, stream, a.vc, enc, stopChan, nil)
	if err != nil {
		return err
	}
	if interrupted {
		// Same treatment as Deliver's own barge-in case: a human talking
		// over a spoken prompt is a resolved outcome, not a failure — and
		// not retryable, since the human already heard however much of it
		// played before interrupting.
		log.Printf("discord-voice: interim prompt playback interrupted by barge-in for request %s", requestID)
	} else if err := a.vc.Speaking(false); err != nil {
		log.Printf("discord-voice: failed to clear speaking state: %v", err)
	}

	if _, err := a.pool.Exec(ctx,
		"UPDATE user_input_requests SET prompt_delivered_at = now() WHERE request_id = $1", requestID,
	); err != nil {
		return err
	}
	log.Printf("discord-voice: pushed pending request %s prompt via connection %s", requestID, a.connectionID)
	return nil
}

// streamPCMToOpus reads Kokoro's raw 24kHz mono PCM off stream one Discord
// frame at a time, converts (upsample 2x, mono->stereo), Opus-encodes, and
// sends each frame to vc.OpusSend — racing every send against ctx.Done()
// and stopChan (voice_bargein.go's fast-path barge-in). Shared by Deliver
// above (a whole turn's response) and DeliverChunk (deliver_voice_chunk.go,
// one streamed sentence) — 2026-08-26, extracted when chunk-level voice
// streaming was added: the two differ only in how much text was handed to
// synthesizeSpeechPCM before this is called, never in how the resulting
// audio gets converted or sent.
//
// interrupted=true means stopChan fired before the stream was fully
// drained — a deliberate stop, not a failure (the caller is responsible for
// its own barge-in bookkeeping and idempotency-marking; this function only
// reports what happened). A non-nil error means a genuine read, encode, or
// send failure — retryable by the caller's own idempotency contract, unlike
// the interrupted case.
// onFirstFrame, if non-nil, is called exactly once, immediately before the
// first frame is actually handed to vc.OpusSend — docs/components/gateway/
// discord-voice.md's Notes Log real-latency-metrics work: this is the one
// place in the whole pipeline that genuinely knows "audio just started
// playing," which both Deliver and DeliverChunk need for their own
// first-audio-latency measurement (a callback rather than doing it here
// directly, since this function has no idea whether it's serving a whole
// turn or one chunk, or whether this particular call is even the actual
// first one for the turn — that's the caller's own voiceLatencyTracker
// bookkeeping to do).
func streamPCMToOpus(ctx context.Context, stream io.Reader, vc *discordgo.VoiceConnection, enc *gopus.Encoder, stopChan <-chan struct{}, onFirstFrame func()) (interrupted bool, err error) {
	// One mono frame's worth of bytes at Kokoro's REAL native rate
	// (kokoroSampleRate, 24kHz — not voiceSampleRate/48kHz: sample_rate
	// isn't honored server-side, confirmed live 2026-08-26, see
	// voice_convert.go's monoToStereoPCM comment) — read directly off the
	// streaming response body, so encoding+sending starts as soon as the
	// first frame's bytes have arrived, not once the whole response has
	// downloaded. Same 20ms cadence as Discord's own frame contract, just
	// at half the sample count until upsample2xPCM below doubles it back.
	const kokoroFrameSamples = voiceFrameSize * kokoroSampleRate / voiceSampleRate
	frameBytes := make([]byte, kokoroFrameSamples*2)
	first := true
	for {
		n, readErr := io.ReadFull(stream, frameBytes)
		if n > 0 {
			// A short final read (io.ErrUnexpectedEOF) still gets encoded —
			// zero-pad the tail to a full frame rather than dropping the
			// last fraction of a second of speech.
			chunk := frameBytes
			if n < len(frameBytes) {
				chunk = make([]byte, len(frameBytes))
				copy(chunk, frameBytes[:n])
			}
			mono24k := pcmBytesToInt16(chunk)
			mono48k := upsample2xPCM(mono24k)
			stereo := monoToStereoPCM(mono48k)
			opusData, encErr := encodeVoiceFrame(enc, stereo)
			if encErr != nil {
				return false, encErr
			}
			if first && onFirstFrame != nil {
				onFirstFrame()
			}
			first = false
			select {
			case <-ctx.Done():
				_ = vc.Speaking(false)
				return false, ctx.Err()
			case <-stopChan:
				_ = vc.Speaking(false)
				return true, nil
			case vc.OpusSend <- opusData:
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return false, nil
			}
			return false, readErr
		}
	}
}
