package main

import (
	"log"
	"sync"
)

// voiceLifecycle — docs/components/gateway/discord-voice.md's "Resolved:
// Turn Lifecycle Stays Gateway-Local": a richer approximation of "what is
// this voice connection doing right now", deliberately NOT persisted to
// the shared turns table — Media Plane state, not Agent Plane state
// (docs/components/gateway.md's own plane split). In-memory only, logged
// on every transition since there's no other observability surface for it
// yet (no admin API, no dedicated table) — cheap, real, and enough to
// answer "what was the bot doing" from pod logs.
//
// Connection-scoped, not per-turn or per-speaker — a real, named
// simplification: multiple speakers can genuinely have independent
// utterances overlapping (discord_voice.go's per-SSRC speakerBuffer
// already handles that correctly for capture itself), but this tracks one
// "what's roughly happening right now" value for the whole connection.
// Good enough for logging/debugging today; revisit if that imprecision
// turns out to matter once there's real usage to look at.
type voiceLifecycleState string

const (
	voiceLifecycleListening    voiceLifecycleState = "listening"
	voiceLifecycleSpeaking     voiceLifecycleState = "speaking"
	voiceLifecycleProcessing   voiceLifecycleState = "processing"
	voiceLifecycleGenerating   voiceLifecycleState = "generating"
	voiceLifecycleSynthesizing voiceLifecycleState = "synthesizing"
	voiceLifecyclePlaying      voiceLifecycleState = "playing"
	voiceLifecycleInterrupted  voiceLifecycleState = "interrupted"
)

type voiceLifecycle struct {
	mu           sync.Mutex
	state        voiceLifecycleState
	connectionID string // log correlation only
}

func newVoiceLifecycle(connectionID string) *voiceLifecycle {
	return &voiceLifecycle{state: voiceLifecycleListening, connectionID: connectionID}
}

// transitionTo moves to a new state and logs the transition. A repeated
// call into the same state (e.g. a second speaker also triggering
// "speaking" while the first is still talking) is not an error — the log
// line is simply skipped for a no-op transition, since it carries no new
// information.
func (l *voiceLifecycle) transitionTo(next voiceLifecycleState) {
	l.mu.Lock()
	prev := l.state
	l.state = next
	l.mu.Unlock()
	if prev != next {
		log.Printf("discord-voice: connection %s lifecycle %s -> %s", l.connectionID, prev, next)
	}
}
