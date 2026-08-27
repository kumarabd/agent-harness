package main

import (
	"context"
	"errors"
	"io"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
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

	// 007_voice_streaming_delivery.sql's own comment: mirrors DiscordDeliver's
	// streamed_message_ref check — if this turn's response was already
	// played progressively via DeliverChunk, re-synthesizing and replaying
	// the whole thing here would talk over what the user already heard.
	var voiceStreamed bool
	if err := a.pool.QueryRow(ctx,
		"SELECT voice_streamed FROM turns WHERE turn_id = $1", turnID,
	).Scan(&voiceStreamed); err != nil {
		return err
	}
	if voiceStreamed {
		return markDelivered()
	}

	var content string
	err := a.pool.QueryRow(ctx,
		"SELECT content FROM messages WHERE parent_id = $1 AND role = 'assistant' ORDER BY seq DESC LIMIT 1",
		turnID,
	).Scan(&content)
	if err != nil {
		return err
	}
	if content == "" {
		// docs/future-work.md §4 — a real, separately-tracked gap (the
		// model sometimes ends a turn with no real content). Nothing to
		// synthesize; not this activity's job to paper over it. Still a
		// genuine resolution, not a failure — mark delivered so a rare
		// duplicate dispatch doesn't re-run this check pointlessly.
		return markDelivered()
	}

	a.lifecycle.transitionTo(voiceLifecycleSynthesizing)
	stream, err := synthesizeSpeechPCM(ctx, content)
	if err != nil {
		return err
	}
	defer stream.Close()

	enc, err := newVoiceOpusEncoder()
	if err != nil {
		return err
	}

	// Fast-path barge-in window: brackets only the actual audio-sending
	// loop below, not the DB reads/TTS request above it — nothing is
	// playing yet during synthesis, so there's nothing for a barge-in
	// signal to usefully stop before this point.
	stopChan := a.bargeIn.startPlayback()
	defer a.bargeIn.endPlayback()
	a.lifecycle.transitionTo(voiceLifecyclePlaying)

	interrupted, err := streamPCMToOpus(ctx, stream, a.vc, enc, stopChan)
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
func streamPCMToOpus(ctx context.Context, stream io.Reader, vc *discordgo.VoiceConnection, enc *gopus.Encoder, stopChan <-chan struct{}) (interrupted bool, err error) {
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
