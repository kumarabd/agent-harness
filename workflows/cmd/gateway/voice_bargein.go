package main

import "sync"

// voiceBargeIn is docs/components/gateway.md's "Resolved: Voice Platforms
// — Cascaded Architecture" fast-path barge-in, closed 2026-08-25: a purely
// local, in-process signal between the Media Gateway's capture loop
// (voiceCaptureLoop, the sender) and the Audio Player's send loop
// (voiceDeliverActivity.Deliver, the receiver) — no Temporal, no Postgres,
// nothing durable, because durability is exactly what makes the existing
// slow path (turn.go's deliverConnectionBased/deliveryInterruptSource) too
// slow to be the thing that stops the bot talking. The two are
// complementary, not alternatives: fast-path only ever stops local audio
// output; deciding what happens next (a new turn, incorporating whatever
// the human just said) is still entirely the slow path's job, unchanged by
// this file.
//
// One instance per voice connection (discord_voice.go's
// activeVoiceConnection), shared by construction — not looked up, not
// addressed by any id, since both sides of this signal live in the exact
// same process for the exact same connection.
type voiceBargeIn struct {
	mu       sync.Mutex
	stopChan chan struct{} // non-nil only while a Deliver call is actively sending frames
}

func newVoiceBargeIn() *voiceBargeIn {
	return &voiceBargeIn{}
}

// startPlayback marks this connection as actively sending audio and returns
// the channel the sender should select on alongside ctx.Done() and
// OpusSend — a signal on this channel means "stop now," decided by VAD
// detecting speech, not by anything Temporal-mediated. Must be paired with
// a deferred endPlayback() call by the same Deliver invocation.
func (b *voiceBargeIn) startPlayback() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	ch := make(chan struct{}, 1)
	b.stopChan = ch
	return ch
}

// endPlayback clears the "actively sending" state — called whether playback
// finished naturally or was stopped by a barge-in signal, so a later
// signalSpeech call (from ordinary conversation once the bot has stopped
// talking) doesn't find a stale channel to write to.
func (b *voiceBargeIn) endPlayback() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopChan = nil
}

// signalSpeech is called by voiceCaptureLoop on every frame the VAD
// classifies as speech — a no-op unless this connection is currently
// playing (checked internally, not the caller's job), and safe to call
// repeatedly for the same ongoing utterance: a buffered, non-blocking send
// rather than a channel close, so there's no double-close panic risk from
// firing on every speech frame rather than just the first one.
func (b *voiceBargeIn) signalSpeech() {
	b.mu.Lock()
	ch := b.stopChan
	b.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- struct{}{}:
	default:
	}
}
