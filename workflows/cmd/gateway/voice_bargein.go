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
//
// Also the connection's single playback-ownership token, since 2026-08-27's
// filler-injection preemption (startPlayback/endPlayback's own comments):
// at most one holder — real delivery (Deliver/DeliverChunk) or a filler
// phrase (voice_filler_player.go) — is ever actually sending audio at a
// time, and this is what arbitrates that, not just what carries the
// stop-for-barge-in signal.
type voiceBargeIn struct {
	mu       sync.Mutex
	stopChan chan struct{} // non-nil only while some player currently holds playback
}

func newVoiceBargeIn() *voiceBargeIn {
	return &voiceBargeIn{}
}

// startPlayback marks this connection as actively sending audio and returns
// the channel the sender should select on alongside ctx.Done() and
// OpusSend — a signal on this channel means "stop now," decided by VAD
// detecting speech, not by anything Temporal-mediated. Must be paired with
// a deferred endPlayback(ch) call (ch being the exact value this returned)
// by the same sender.
//
// Preemption, added 2026-08-27 for filler injection (docs/components/
// gateway/discord-voice.md's Notes Log): if a previous caller's stopChan is
// still outstanding — a filler phrase still playing when the real response
// is ready — it's closed here immediately, rather than left to linger.
// Before filler injection, at most one caller ever held playback at a time
// (Deliver/DeliverChunk dispatch strictly sequentially, always pairing
// startPlayback with endPlayback before the next call), so stopChan was
// always nil by the time a new startPlayback happened — this branch is a
// pure no-op for every pre-existing call site, only doing real work for the
// new filler-preemption case.
func (b *voiceBargeIn) startPlayback() <-chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopChan != nil {
		close(b.stopChan)
	}
	ch := make(chan struct{}, 1)
	b.stopChan = ch
	return ch
}

// endPlayback clears the "actively sending" state — called whether playback
// finished naturally, was stopped by a barge-in signal, or was preempted by
// a later startPlayback call — so a later signalSpeech call (from ordinary
// conversation once the bot has stopped talking) doesn't find a stale
// channel to write to.
//
// Compare-and-clear, not unconditional — changed 2026-08-27 alongside the
// preemption above: a caller that was PREEMPTED (its own stopChan already
// closed and replaced by someone else's startPlayback, e.g. a filler phrase
// that lost the race to the real response) must not clobber whoever
// preempted it when its own deferred cleanup eventually runs. mine is the
// exact channel this same caller's own startPlayback call returned — every
// caller already has it in scope for its own select anyway, so this asks
// nothing new of them beyond passing it back.
func (b *voiceBargeIn) endPlayback(mine <-chan struct{}) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopChan == mine {
		b.stopChan = nil
	}
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
