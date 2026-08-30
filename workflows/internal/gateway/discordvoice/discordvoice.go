// Package discordvoice is the Discord voice-channel gateway platform:
// speech-to-text ingestion off a live RTP voice connection and
// text-to-speech delivery back onto it (docs/components/gateway/
// discord-voice.md). It owns the whole voice DSP stack (VAD, end-of-turn
// detection, Opus, barge-in, filler injection, latency metrics) and the
// VoiceDeliver* Temporal activities.
//
// It does NOT own the Discord connection — the discord package holds the
// single *discordgo.Session (a voice connection is a child of the open text
// gateway session) and delegates /join, /leave, and voice interactions here.
package discordvoice

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/lease"
)

// Voice is the Discord voice platform. One per Discord bot (the discord
// package constructs it and hands it the shared session at interaction
// time).
type Voice struct {
	ingestor    *core.Ingestor
	pool        *pgxpool.Pool
	temporal    client.Client
	leases      *lease.Manager
	state       *voiceState
	fillerCache *voiceFillerCache
}

// New wires a Voice platform and pre-synthesizes the filler-phrase cache
// (best-effort — a kokoro-svc hiccup at startup degrades to no filler, not a
// failed construction; see synthesizeVoiceFillerCache).
func New(ctx context.Context, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, leases *lease.Manager) *Voice {
	return &Voice{
		ingestor:    ingestor,
		pool:        pool,
		temporal:    temporal,
		leases:      leases,
		state:       newVoiceState(),
		fillerCache: synthesizeVoiceFillerCache(ctx),
	}
}

// TeardownAll leaves every voice channel this Voice currently holds and
// releases their leases — called by the discord package on process
// shutdown, so leases are freed promptly rather than waiting out their TTL.
func (v *Voice) TeardownAll(ctx context.Context) {
	v.state.mu.Lock()
	keys := make([]string, 0, len(v.state.byKey))
	for k := range v.state.byKey {
		keys = append(keys, k)
	}
	v.state.mu.Unlock()
	for _, k := range keys {
		v.teardownVoiceConnection(ctx, k, "gateway shutdown")
	}
}
