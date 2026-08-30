package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
)

// docs/components/gateway/discord-voice.md — Discord voice channel
// ingestion (speech-to-text) and delivery (text-to-speech), the first
// voice-capable Gateway platform kind. Join/leave are command-triggered
// (/join, /leave), not automatic; each joined voice channel is its own
// live connection, downstream of the already-open text gateway connection
// this file shares a *discordgo.Session with (discord.go's dg).

// voiceConnectionLeaseTTL — same numeric-tuning discipline as
// discordConnectionLeaseTTL: a real starting value, not tuned.
const voiceConnectionLeaseTTL = 30 * time.Second

// voiceUtteranceSilenceFrames bounds how many consecutive silent 20ms
// frames end an utterance — the fail-OPEN ceiling on how long the pipeline
// will wait for a person to keep talking, reached whenever the EOT model
// (below) doesn't confidently say "done" first, OR isn't configured at all.
//
// 70 frames / 1400ms, raised 2026-08-29 from 35 / 700ms (docs/components/
// gateway/discord-voice.md's Notes Log — first real tuning pass on the
// "VAD threshold tuning" open item, against real conversation reports of
// the bot talking over the user). 700ms is shorter than a natural
// mid-thought pause — people routinely stop for a second or more while
// composing the rest of a sentence — and since the EOT model as wired can
// only end an utterance EARLIER than this ceiling, never later, this number
// IS the whole patience budget. Deliberately generous as a starting point;
// tune down against real usage if turns feel laggy.
const voiceUtteranceSilenceFrames = 70

// voiceUtteranceMinSpeechFrames — real, live bug fixed 2026-08-27: barge-in
// and utterance accumulation used to fire on the very FIRST VAD-classified-
// speech frame, unconditionally, with no debounce at all. Even Silero
// (already enabled for this tenant, a real neural VAD, not just an energy
// threshold) can briefly misclassify a click, cough, or background noise
// burst as speech for a frame or two — with no minimum-sustained-speech
// requirement, that alone was enough to interrupt playback and start
// accumulating a would-be utterance. Requires speech to actually persist
// before treating it as real, matching the attack-time hysteresis real VAD
// systems commonly use. Deliberately much shorter than
// voiceUtteranceSilenceFrames: confirming speech STARTED should be fast, only
// deciding it ENDED needs the long tail to tolerate a natural mid-sentence
// pause.
//
// 18 frames (360ms), raised 2026-08-29 from 13 / 260ms (same tuning pass as
// voiceUtteranceSilenceFrames above). 260ms came from matching Silero VAD's
// own reference `min_speech_duration_ms` (~250ms) — a good floor for "is
// this speech at all," but this constant also gates BARGE-IN (the bot
// yielding the floor mid-response), and 260ms of any sound was enough to
// cut the bot off. 360ms asks for a bit more sustained speech before the
// bot stops talking, without being anywhere near long enough to clip a real
// interruption. Still far shorter than voiceUtteranceSilenceFrames'
// 1400ms — confirming speech STARTED should be quick; only deciding it
// ENDED needs the long tail to tolerate a natural mid-sentence pause. Not
// the same thing as Silero's own `min_silence_duration_ms` (~100ms), which
// governs merging adjacent speech blips within general-purpose segmentation.
const voiceUtteranceMinSpeechFrames = 18

// voiceUtteranceEOTGraceFrames — docs/components/gateway/discord-voice.md's
// "Resolved: Turn-Taking Model": the shortest silence worth even asking the
// EOT sidecar about. 25 frames (500ms), raised 2026-08-29 from 10 / 200ms
// (same tuning pass as voiceUtteranceSilenceFrames above). LiveKit's own
// `MIN_SILENCE_DURATION_MS` reference is 200ms, but a confident-sounding
// clause followed by a 200ms breath ("what's the weather" <breath> "in San
// Francisco") was scoring over eotThreshold and flushing before the person
// finished — so the model never got a chance to see the rest of the
// sentence. 500ms is past a normal inter-word breath but still well under a
// deliberate mid-thought pause, so a genuinely finished sentence still gets
// a response about half a second after the person stops.
const voiceUtteranceEOTGraceFrames = 25

// voiceUtteranceEOTCheckIntervalFrames throttles how often predict() is
// actually called once past the grace period — every silent frame would be
// a real gRPC call every 20ms for up to (voiceUtteranceSilenceFrames -
// voiceUtteranceEOTGraceFrames) frames per utterance tail; every 5th frame
// (100ms) cuts that ~5x while staying well under what a human would
// perceive as added delay. A real, reasoned throttle, not a tuned value —
// same "real starting point, not tuned" category as every other numeric
// constant in this file.
const voiceUtteranceEOTCheckIntervalFrames = 5

// voiceState tracks this Gateway replica's own live voice connections —
// in-memory only, not Postgres: gateway_connection_leases is the durable
// record of who currently holds each connection (docs/components/gateway.md's
// "Resolved: Connection Leasing"), but the live *discordgo.VoiceConnection
// object itself is process-local, same as discord.go's own dg session.
type voiceState struct {
	mu    sync.Mutex
	byKey map[string]*activeVoiceConnection // key: guildID + ":" + botConnectionID — see voiceJoin's own comment on why guildID alone isn't enough once multiple bots exist
}

type activeVoiceConnection struct {
	vc           *discordgo.VoiceConnection
	connectionID string // gateway.md's composable connection_id: {bot_id}:{voice_channel_id}
	holderID     string // the bot's own id — leases.go's holder_id, stored directly rather than re-parsed out of connectionID
	cancel       context.CancelFunc
	deliverWkr   worker.Worker
}

func newVoiceState() *voiceState {
	return &voiceState{byKey: make(map[string]*activeVoiceConnection)}
}

// registerVoiceCommands — docs/components/gateway/discord-voice.md's
// "Resolved: Join Trigger": /join and /leave, not auto-join/auto-leave.
// Registered globally (guildID "" — every guild the bot is in gets both
// commands). appID is the bot's own resolved user id (discord.go's
// connectionID) — true for a standard single-application bot token, and
// avoids depending on dg.State.User, which isn't populated until after
// dg.Open() succeeds (this is called before that).
func (s *server) registerVoiceCommands(dg *discordgo.Session, appID string) {
	commands := []*discordgo.ApplicationCommand{
		{Name: "join", Description: "Join the voice channel you're currently in"},
		{Name: "leave", Description: "Leave the voice channel this bot is in"},
		// docs/components/gateway/discord.md's "Resolved: Per-Channel Reply
		// Mode" — not voice-channel-specific (it governs text-channel/DM
		// replies), just registered here alongside the other slash commands.
		{
			Name:        "mode",
			Description: "Choose whether I reply with text or a voice message in this channel",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionString,
					Name:        "reply",
					Description: "voice or text",
					Required:    true,
					Choices: []*discordgo.ApplicationCommandOptionChoice{
						{Name: "voice", Value: replyModeVoice},
						{Name: "text", Value: replyModeText},
					},
				},
			},
		},
	}
	for _, cmd := range commands {
		if _, err := dg.ApplicationCommandCreate(appID, "", cmd); err != nil {
			log.Printf("discord-voice: failed to register /%s command: %v", cmd.Name, err)
		}
	}
}

// shutdownCtx is the process's own shutdown-tied context (discord.go's
// startDiscordPlatform closure) — used for the short-lived DB call this
// handler itself makes, and passed into voiceJoin as the parent for that
// connection's own long-running goroutines, so a voice connection's
// lifetime is tied to the process's, not to context.Background().
func (s *server) discordVoiceInteractionCreate(shutdownCtx context.Context, dg *discordgo.Session, ic *discordgo.InteractionCreate, connectionID string) {
	if ic.Type != discordgo.InteractionApplicationCommand {
		return
	}
	name := ic.ApplicationCommandData().Name
	if name != "join" && name != "leave" {
		return
	}

	var content string
	switch name {
	case "join":
		content = s.voiceJoin(shutdownCtx, dg, ic, connectionID)
	case "leave":
		content = s.voiceLeave(shutdownCtx, ic, connectionID)
	}

	if err := dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content},
	}); err != nil {
		log.Printf("discord-voice: failed to respond to /%s interaction: %v", name, err)
	}
}

func (s *server) voiceJoin(ctx context.Context, dg *discordgo.Session, ic *discordgo.InteractionCreate, botConnectionID string) string {
	if ic.GuildID == "" {
		return "Voice channels don't exist in DMs."
	}
	var userID string
	if ic.Member != nil && ic.Member.User != nil {
		userID = ic.Member.User.ID
	}
	vstate, err := dg.State.VoiceState(ic.GuildID, userID)
	if err != nil || vstate == nil || vstate.ChannelID == "" {
		return "Join a voice channel first, then run /join."
	}
	channelID := vstate.ChannelID

	// docs/components/gateway/discord-voice.md's "Resolved: Silero VAD" —
	// fail loud: refuse a new join outright if Silero is configured for
	// this deployment but its sidecar isn't reachable, rather than
	// starting a connection whose VAD is already known to be broken.
	if url := vadSidecarURL(); url != "" {
		healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		healthy := newSileroVADClient(url).healthy(healthCtx)
		cancel()
		if !healthy {
			return "Voice activity detection is currently unavailable — try /join again shortly."
		}
	}

	// gateway.md's composable connection_id (2026-08-25 amendment):
	// {bot_id}:{voice_channel_id} — addresses this specific connection, not
	// "this bot" as a whole, since the same bot can hold a simultaneous
	// voice connection in every guild it's in.
	connectionID := botConnectionID + ":" + channelID
	holderID := botConnectionID
	ok, err := s.acquireOrRenewConnectionLease(ctx, "discord-voice", connectionID, holderID, voiceConnectionLeaseTTL)
	if err != nil {
		log.Printf("discord-voice: lease acquire error: %v", err)
		return "Couldn't join — an internal error occurred."
	}
	if !ok {
		return "Already connected to a voice channel elsewhere (or another replica is handling it)."
	}

	// mute=false, deaf=false — this bot's entire purpose is to receive and
	// process incoming audio (VAD, STT); `deaf=true` tells Discord's own
	// servers to withhold audio from this connection entirely at the
	// protocol level, not just a client-side UI toggle. Confirmed live:
	// this was the exact cause of the bot showing deafened in Discord and
	// receiving no audio at all regardless of who was speaking.
	vc, err := dg.ChannelVoiceJoin(ctx, ic.GuildID, channelID, false, false)
	if err != nil {
		_ = s.releaseConnectionLease(ctx, "discord-voice", connectionID, holderID)
		log.Printf("discord-voice: failed to join channel %s: %v", channelID, err)
		return "Couldn't join that voice channel."
	}

	// Derived from ctx (the process's real shutdown-tied context, passed in
	// from discordVoiceInteractionCreate), not context.Background() — this
	// connection's own renewal/capture goroutines need to actually stop on
	// SIGTERM, not be orphaned past process shutdown.
	connCtx, cancel := context.WithCancel(ctx)

	// docs/components/gateway.md's "Resolved: Voice Platforms — Cascaded
	// Architecture" fast-path barge-in: one instance per connection, shared
	// by construction between the capture loop (sender, below) and the
	// delivery activity (receiver) — nothing else needs to reach it, so it
	// isn't stored on activeVoiceConnection.
	bargeIn := newVoiceBargeIn()
	// docs/components/gateway/discord-voice.md's "Resolved: Turn Lifecycle
	// Stays Gateway-Local" — same sharing pattern as bargeIn above: one
	// instance per connection, passed directly to both sides that need it.
	lifecycle := newVoiceLifecycle(connectionID)
	// latency — docs/components/gateway/discord-voice.md's Notes Log, "get a
	// real TTFB number so we're not guessing". Same one-instance-per-
	// connection, shared-between-capture-and-delivery treatment as bargeIn/
	// lifecycle above.
	latency := newVoiceLatencyTracker()
	// filler — docs/components/gateway/discord-voice.md's Notes Log, filler
	// injection. Per-connection like bargeIn/lifecycle/latency above, but
	// only ever used from voiceCaptureLoop's own flush (the trigger side) —
	// deliverActivity never needs a reference to it at all, since
	// voiceBargeIn's own preemption is what stops a still-playing filler
	// once Deliver/DeliverChunk's already-existing startPlayback call
	// happens, not anything this struct coordinates itself.
	filler := newVoiceFillerPlayer(s.voiceFillerCache)
	deliverActivity := &voiceDeliverActivity{
		vc: vc, pool: s.pool, connectionID: connectionID,
		// session + voiceChannelID — docs/components/user-input.md,
		// "Resolved: Response-Routing for Discord Voice — Reuse the
		// Text Button Flow". DeliverInterim needs both to push a text
		// message with buttons to the voice channel's attached text
		// chat alongside the spoken prompt (Discord's voice channels
		// have unified voice+text since 2022 — same channel ID for
		// both). vc alone can send audio but not text; session is the
		// parent that opened this connection.
		session: dg, voiceChannelID: channelID,
		bargeIn: bargeIn, lifecycle: lifecycle, latency: latency,
	}
	deliverWkr := worker.New(s.temporal, "deliver:discord-voice:"+connectionID, worker.Options{DisableWorkflowWorker: true})
	deliverWkr.RegisterActivityWithOptions(deliverActivity.Deliver, activity.RegisterOptions{Name: "VoiceDeliver"})
	deliverWkr.RegisterActivityWithOptions(deliverActivity.DeliverChunk, activity.RegisterOptions{Name: "VoiceDeliverChunk"})
	// docs/components/user-input.md's "Mid-turn interim delivery" (push
	// half, A+B) — same embedded worker, same live voice connection.
	deliverWkr.RegisterActivityWithOptions(deliverActivity.DeliverInterim, activity.RegisterOptions{Name: "VoiceDeliverInterim"})
	if err := deliverWkr.Start(); err != nil {
		log.Printf("discord-voice: failed to start embedded delivery worker: %v", err)
	}

	// Keyed (guildID, botConnectionID), not guildID alone — multi-bot
	// support (values.yaml's gateway.discord.bots list) means two DIFFERENT
	// bots can each independently hold their own voice connection in the
	// SAME guild; keying by guildID alone would let one bot's join silently
	// clobber another's registry entry.
	voiceKey := ic.GuildID + ":" + botConnectionID
	s.voice.mu.Lock()
	existing, hadExisting := s.voice.byKey[voiceKey]
	s.voice.byKey[voiceKey] = &activeVoiceConnection{vc: vc, connectionID: connectionID, holderID: holderID, cancel: cancel, deliverWkr: deliverWkr}
	s.voice.mu.Unlock()

	if hadExisting {
		existing.cancel()
		if existing.deliverWkr != nil {
			existing.deliverWkr.Stop()
		}
		_ = existing.vc.Disconnect(ctx)
		// Release promptly rather than leaving it to the lease's own TTL —
		// this replica genuinely no longer holds it, no reason to make
		// another replica (or a later /join in this same guild) wait out
		// the expiry window.
		_ = s.releaseConnectionLease(ctx, "discord-voice", existing.connectionID, existing.holderID)
	}

	go s.voiceLeaseRenewalLoop(connCtx, connectionID, holderID)
	go s.voiceCaptureLoop(connCtx, vc, channelID, connectionID, voiceKey, bargeIn, lifecycle, latency, filler)

	return "Joined."
}

func (s *server) voiceLeave(ctx context.Context, ic *discordgo.InteractionCreate, botConnectionID string) string {
	if ic.GuildID == "" {
		return "Voice channels don't exist in DMs."
	}
	voiceKey := ic.GuildID + ":" + botConnectionID
	if !s.teardownVoiceConnection(ctx, voiceKey, "user requested /leave") {
		return "Not currently in a voice channel here."
	}
	return "Left the voice channel."
}

// teardownVoiceConnection removes and cleanly tears down whatever
// connection is registered under voiceKey — the shared mechanism behind
// both a user-requested /leave and a fail-loud teardown triggered by a
// broken VAD sidecar (voiceCaptureLoop, docs' "Resolved: Silero VAD").
// Returns false if there was nothing registered under voiceKey to tear
// down.
func (s *server) teardownVoiceConnection(ctx context.Context, voiceKey, reason string) bool {
	s.voice.mu.Lock()
	entry, ok := s.voice.byKey[voiceKey]
	if ok {
		delete(s.voice.byKey, voiceKey)
	}
	s.voice.mu.Unlock()
	if !ok {
		return false
	}

	entry.cancel()
	if entry.deliverWkr != nil {
		entry.deliverWkr.Stop()
	}
	_ = entry.vc.Disconnect(ctx)
	_ = s.releaseConnectionLease(ctx, "discord-voice", entry.connectionID, entry.holderID)
	log.Printf("discord-voice: connection %s torn down (%s)", entry.connectionID, reason)
	return true
}

// voiceLeaseRenewalLoop mirrors discord.go's runDiscordConnection renewal
// loop, scoped to one voice connection instead of the whole bot's text
// connection.
func (s *server) voiceLeaseRenewalLoop(ctx context.Context, connectionID, holderID string) {
	ticker := time.NewTicker(voiceConnectionLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.acquireOrRenewConnectionLease(ctx, "discord-voice", connectionID, holderID, voiceConnectionLeaseTTL)
			if err != nil {
				log.Printf("discord-voice: lease renew error: %v", err)
				continue
			}
			if !ok {
				log.Printf("discord-voice: lost connection lease for %s", connectionID)
				return
			}
		}
	}
}

// speakerBuffer accumulates one speaker's in-progress utterance.
type speakerBuffer struct {
	pcm           []int16
	silenceFrames int
	// speechFrames/pendingSpeechPCM — debounce state for a not-yet-confirmed
	// speech onset (voiceUtteranceMinSpeechFrames' own comment has the real
	// bug this closes: a single VAD-classified-speech frame, even from a
	// real neural VAD, used to fire barge-in and start an utterance
	// immediately — a click, a cough, a stray noise burst was enough).
	// pendingSpeechPCM holds frames provisionally while speechFrames is
	// still below threshold, so a genuine utterance's actual onset isn't
	// clipped once it IS confirmed — see the capture loop's own comment for
	// the promote-on-confirm mechanics. Both reset to zero/nil the instant
	// a silent frame arrives before confirmation (that run wasn't real
	// speech); neither is touched once len(pcm) > 0 (a confirmed utterance
	// already in progress uses the same unchanged pcm/silenceFrames path as
	// before this existed).
	speechFrames     int
	pendingSpeechPCM []int16
	// startedDuringPlayback is captured once, at the exact moment this
	// utterance is confirmed (the same promotion point that sets speech
	// frames/pcm above), not re-checked at flush time — bargeIn.isPlaying()
	// answers "is something playing right now," and by flush time playback
	// has usually already been preempted by this very utterance's own
	// signalSpeech() call, so checking then would almost always read false
	// regardless of what was actually true when the user started talking.
	// voice_backchannel.go's own comment has the real bug this closes.
	startedDuringPlayback bool
	// eotFailureLogged suppresses repeat log lines for one utterance's
	// silence tail — the EOT sidecar's own predict() is polled a handful of
	// times per utterance (see voiceUtteranceEOTCheckIntervalFrames), and an
	// unreachable sidecar would otherwise log identically on every one of
	// them. Reset to false on flush, next to the other per-utterance fields.
	eotFailureLogged bool
	// vad is one instance per speaker, not shared across the connection —
	// required for Silero (voice_vad_silero.go), whose ONNX model carries
	// real per-stream RNN state that must not mix between independent
	// speakers. energyVAD has no state to protect, but gets its own
	// instance too rather than special-casing one VAD kind over another.
	vad voiceActivityDetector
	// stt is this speaker's own WhisperLive streaming session
	// (voice_stt_realtime.go) — nil when WHISPERLIVE_URL isn't configured,
	// in which case utterances go entirely through the batch transcribeAudio
	// path (voice_stt_tts.go). One session per speaker: WhisperLive's own
	// recognizer state is real per-connection state that must not mix
	// between speakers, same reasoning as vad above.
	//
	// The session is opened when this buffer is created (so its WebSocket
	// handshake overlaps with the pre-utterance silence and no audio is
	// lost), but it is only FED audio while an utterance is active — see
	// feedSTT. Feeding it continuously (the pre-2026-08-29 design) made it
	// run large-v3 inference on ambient room tone 24/7 for the whole time
	// the bot was joined, pinning the shared GPU; docs/components/gateway/
	// discord-voice.md's "Resolved: Gated Streaming STT Feed".
	stt *whisperLiveSession
}

// feedSTT streams one Discord frame (48kHz stereo int16) to this speaker's
// WhisperLive session, downmixed/resampled to the 16kHz mono float32 the
// session expects (reusing voice_vad_silero.go's downmixResample). A no-op
// when no session is configured, and a no-op inside sendAudio itself until
// the handshake completes.
//
// MUST only be called while an utterance is active — a confirmed-speech
// frame, the promoted lead-in, or a trailing-silence frame during the
// countdown to flush. Never on the dead air between utterances: that is
// exactly the feed that pinned the GPU. The trailing-silence frames ARE
// fed on purpose — WhisperLive's own server-side VAD needs to see the gap
// in the audio to finalize the segment, and ~1.4s of it (the silence
// ceiling) is plenty.
func (b *speakerBuffer) feedSTT(frame []int16) {
	if b.stt != nil {
		b.stt.sendAudio(downmixResample(frame))
	}
}

// voiceCaptureLoop reads decoded PCM per speaker off vc.OpusRecv, uses the
// VAD to find utterance boundaries (docs/components/gateway/discord-voice.md's
// "Resolved: Audio Pipeline Shape"), and on each completed utterance,
// transcribes and submits it as a MessageEvent — with no trigger gate at
// all ("Resolved: Trigger Detection — No Gate, Deferred Pending Real Usage"):
// every utterance becomes a real turn.
func (s *server) voiceCaptureLoop(ctx context.Context, vc *discordgo.VoiceConnection, channelID, connectionID, voiceKey string, bargeIn *voiceBargeIn, lifecycle *voiceLifecycle, latency *voiceLatencyTracker, filler *voiceFillerPlayer) {
	dec, err := newVoiceOpusDecoder()
	if err != nil {
		log.Printf("discord-voice: failed to create opus decoder: %v", err)
		return
	}

	// vadFactory is resolved once per connection, not per speaker — every
	// speaker on this connection uses the same VAD kind, just their own
	// independent instance (see speakerBuffer.vad above). Declared against
	// the interface explicitly since the two concrete constructors
	// (*energyVAD vs *sileroVAD) aren't the same function type.
	var vadFactory func() voiceActivityDetector = func() voiceActivityDetector { return newEnergyVAD() }
	if url := vadSidecarURL(); url != "" {
		sileroClient := newSileroVADClient(url)
		vadFactory = func() voiceActivityDetector { return newSileroVAD(sileroClient) }
	}

	// sttFactory is nil (not a func returning nil) when WHISPERLIVE_URL isn't
	// configured — checked at each speakerBuffer's creation so "not
	// configured" and "configured" stay unambiguous. The session it builds
	// is fed audio only while an utterance is active (speakerBuffer.feedSTT),
	// not continuously.
	var sttFactory func() *whisperLiveSession
	if url := whisperLiveURL(); url != "" {
		sttFactory = func() *whisperLiveSession { return newWhisperLiveSession(url) }
	}

	// eot — one shared client for the whole connection, not one per speaker:
	// voice_eot.go's own comment has the reasoning (every call is a fresh,
	// independent classification, no per-speaker state to protect, unlike
	// vadFactory above). Reuses vadSidecarURL() directly rather than a
	// separate env var/Helm value — the EOT service is colocated on the same
	// sidecar process/port as Silero VAD (server.py's own doc comment), so
	// there's only one real target to configure. nil (not a client that
	// always errors) when unconfigured — the fixed voiceUtteranceSilenceFrames
	// timeout is exactly what already runs when this is nil, so "unconfigured"
	// needs no separate code path of its own.
	var eot *eotClient
	if url := vadSidecarURL(); url != "" {
		eot = newEOTClient(url)
	}

	var mu sync.Mutex
	ssrcToUser := make(map[uint32]string)
	buffers := make(map[uint32]*speakerBuffer)

	// On loop exit (ctx cancelled, OpusRecv closed, or a VAD-failure
	// teardown) close every speaker's WhisperLive session so its WebSocket
	// and readLoop goroutine don't leak until the server's own 1h timeout.
	defer func() {
		mu.Lock()
		for _, b := range buffers {
			if b.stt != nil {
				b.stt.close()
			}
		}
		mu.Unlock()
	}()

	vc.AddHandler(func(_ *discordgo.VoiceConnection, vs *discordgo.VoiceSpeakingUpdate) {
		mu.Lock()
		ssrcToUser[uint32(vs.SSRC)] = vs.UserID
		mu.Unlock()
	})

	// flush must only ever be called with mu already held — it's invoked
	// from inside the locked section below, and reads ssrcToUser/buf
	// directly rather than re-locking (mu is not reentrant; re-locking here
	// would deadlock the caller on its very first call).
	flush := func(ssrc uint32, buf *speakerBuffer) {
		if len(buf.pcm) == 0 {
			return
		}
		userID := ssrcToUser[ssrc]
		pcm := buf.pcm
		stt := buf.stt
		startedDuringPlayback := buf.startedDuringPlayback
		buf.pcm = nil
		buf.silenceFrames = 0
		buf.startedDuringPlayback = false
		buf.eotFailureLogged = false
		lifecycle.transitionTo(voiceLifecycleProcessing)

		go func() {
			// Gateway's own VAD/EOT (above) decides the utterance is over.
			// When a WhisperLive session exists, it has been fed this
			// utterance's audio in real time (speakerBuffer.feedSTT, gated on
			// utterance-active — never on the silence between utterances,
			// which is what used to pin the shared GPU), so its segments are
			// already the transcript — read them, don't issue a fresh
			// request. Batch transcribeAudio (voice_stt_tts.go) is the
			// fallback: no session configured, a session error, or the
			// session had nothing for this utterance. The two never run
			// together on the success path.
			var text string
			if stt != nil {
				text = stt.consumeNewText()
				if sttErr := stt.Err(); sttErr != nil {
					log.Printf("discord-voice: whisperlive session error, falling back to batch STT: %v", sttErr)
				}
			}
			if text == "" {
				if stt != nil {
					log.Printf("discord-voice: whisperlive returned no text for this utterance, falling back to batch STT")
				}
				wav := pcmToWAV(pcm, voiceSampleRate, voiceChannels)
				batchText, err := transcribeAudio(context.Background(), wav)
				if err != nil {
					log.Printf("discord-voice: transcription failed: %v", err)
					lifecycle.transitionTo(voiceLifecycleListening)
					return
				}
				text = batchText
			}
			if text == "" {
				lifecycle.transitionTo(voiceLifecycleListening)
				return
			}
			if isFillerOnly(text) {
				// voice_filler.go's own comment has the full reasoning —
				// the downstream half of the debounce fix: a genuinely,
				// clearly vocalized "um" can still get transcribed
				// correctly even though it carries no real content. Logged
				// (not silent) so real usage can inform whether
				// voiceFillerWords' set needs extending later.
				log.Printf("discord-voice: filtered filler-only transcript %q, no turn started", text)
				lifecycle.transitionTo(voiceLifecycleListening)
				return
			}
			if startedDuringPlayback && isBackchannelOnly(text) {
				// voice_backchannel.go's own comment has the full reasoning
				// — this is the real bug a live test run surfaced: the user
				// said "Yeah." while the bot was still talking, it wasn't
				// treated as a backchannel, and the dispatched turn came
				// back completely empty. Unlike isFillerOnly above, this
				// check is context-gated: the exact same transcript ("yeah")
				// said when the bot was NOT playing is a real one-word
				// answer, not a backchannel, and must reach submitMessageEvent
				// like any other utterance — startedDuringPlayback is what
				// tells the two cases apart.
				log.Printf("discord-voice: filtered backchannel-during-playback transcript %q, no turn started", text)
				lifecycle.transitionTo(voiceLifecycleListening)
				return
			}
			lifecycle.transitionTo(voiceLifecycleGenerating)
			// docs/components/gateway/discord-voice.md's Notes Log — the
			// real "STT-completion to first-audio-frame" latency starts
			// exactly here: transcription just finished, a real turn is
			// about to be submitted. voiceDeliverActivity (same process,
			// same connection) reads this back at the first audio frame it
			// actually sends.
			latency.markTurnStart()
			// docs/components/gateway/discord-voice.md's Notes Log, filler
			// injection — fired here, not after submitMessageEvent below:
			// the whole point is starting before the real turn even exists
			// yet, masking exactly the dead air voice_first_audio_latency_seconds
			// measures. Its own goroutine (fire-and-forget, same as flush's
			// enclosing one) — this closure has real turn-dispatch work of
			// its own to get on with, not waiting on a filler phrase to
			// finish speaking. A no-op if FILLER_ENABLED isn't set for this
			// deployment (voiceFillerPlayer.start's own cache check).
			go filler.start(ctx, vc, bargeIn)
			event := MessageEvent{
				Platform:          "discord-voice",
				ChannelID:         channelID,
				User:              userID,
				Content:           text,
				PlatformMessageID: time.Now().UTC().Format(time.RFC3339Nano) + ":" + userID,
				Discriminator:     "channel:" + channelID,
				ConnectionID:      connectionID,
			}
			if _, err := s.submitMessageEvent(context.Background(), event); err != nil {
				log.Printf("discord-voice: failed to submit message event: %v", err)
				// No turn actually started, so Deliver's own reset-to-
				// listening (deliver_voice.go) will never run for this
				// attempt — reset here instead.
				lifecycle.transitionTo(voiceLifecycleListening)
			}
		}()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case pkt, more := <-vc.OpusRecv:
			if !more {
				return
			}
			if pkt == nil || len(pkt.Opus) == 0 {
				continue
			}
			pcm, err := decodeVoiceFrame(dec, pkt.Opus)
			if err != nil {
				continue
			}
			mu.Lock()
			buf, ok := buffers[pkt.SSRC]
			if !ok {
				buf = &speakerBuffer{vad: vadFactory()}
				if sttFactory != nil {
					// Opened now, while this speaker is (probably) still
					// silent, so the WebSocket handshake is done by the time
					// they actually start talking. NOT fed here — feedSTT is
					// called only from the utterance-active branches below.
					buf.stt = sttFactory()
				}
				buffers[pkt.SSRC] = buf
			}
			speech := buf.vad.isSpeech(pcm)
			if err := buf.vad.Err(); err != nil {
				// docs/components/gateway/discord-voice.md's "Resolved:
				// Silero VAD" — fail loud, not a silent fallback to
				// energyVAD: this connection's detection is broken, so
				// the connection itself is torn down cleanly rather than
				// continuing on data nobody can trust. Recovery is a
				// fresh /join, matching the existing command-triggered
				// join model, not a silent retry loop.
				mu.Unlock()
				log.Printf("discord-voice: VAD sidecar failed for connection %s, tearing down: %v", connectionID, err)
				s.teardownVoiceConnection(context.Background(), voiceKey, "VAD sidecar failure")
				return
			}
			switch {
			case speech && len(buf.pcm) > 0:
				// Already a confirmed, in-progress utterance — unchanged
				// from before the debounce fix: every speech frame keeps
				// resetting the end-of-utterance silence timer, and barge-in
				// fires on every frame (a no-op unless this connection is
				// currently playing, checked inside signalSpeech itself).
				bargeIn.signalSpeech()
				lifecycle.transitionTo(voiceLifecycleSpeaking)
				buf.pcm = append(buf.pcm, pcm...)
				buf.feedSTT(pcm)
				buf.silenceFrames = 0

			case speech:
				// Not yet confirmed as real speech — accumulate
				// provisionally (voiceUtteranceMinSpeechFrames' own comment
				// has the full reasoning) without acting on it yet: no
				// barge-in, no lifecycle transition, nothing dispatched.
				buf.pendingSpeechPCM = append(buf.pendingSpeechPCM, pcm...)
				buf.speechFrames++
				if buf.speechFrames >= voiceUtteranceMinSpeechFrames {
					// Confirmed. Promote the provisional lead-in into the
					// real buffer so the utterance's actual onset isn't
					// clipped by the frames spent confirming it, then start
					// acting on it exactly like the already-confirmed case
					// above would.
					//
					// startedDuringPlayback is captured here, at confirmation
					// time — voice_backchannel.go's own comment on
					// speakerBuffer has the reasoning for why here and not at
					// flush time.
					buf.startedDuringPlayback = bargeIn.isPlaying()
					bargeIn.signalSpeech()
					lifecycle.transitionTo(voiceLifecycleSpeaking)
					buf.pcm = append(buf.pcm, buf.pendingSpeechPCM...)
					// Feed the whole confirmed lead-in as one chunk — this is
					// the first audio WhisperLive gets for this utterance, and
					// it includes the current frame (already appended to
					// pendingSpeechPCM at the top of this case).
					buf.feedSTT(buf.pendingSpeechPCM)
					buf.pendingSpeechPCM = nil
					buf.speechFrames = 0
					buf.silenceFrames = 0
				}

			case len(buf.pcm) > 0:
				// Confirmed utterance, now silent.
				// voiceUtteranceSilenceFrames stays as a hard, fail-OPEN
				// ceiling: reached either when eot isn't configured for this
				// deployment at all, or when the model never confidently
				// signals "done" before the ceiling — an unreachable EOT
				// sidecar degrades this specific decision back to exactly
				// the fixed timer it used to always be, not a torn-down
				// connection (contrast the VAD sidecar's own fail-loud
				// teardown above: misclassifying real speech as silence is a
				// much costlier mistake than getting FLUSH TIMING wrong).
				//
				// Keep feeding these trailing-silence frames to WhisperLive
				// (but NOT beyond flush) — its server-side VAD needs to see
				// the silence in the audio stream to close off the segment,
				// and the feed genuinely stops once the utterance flushes.
				buf.feedSTT(pcm)
				buf.silenceFrames++
				ended := buf.silenceFrames >= voiceUtteranceSilenceFrames
				if !ended && eot != nil &&
					buf.silenceFrames >= voiceUtteranceEOTGraceFrames &&
					(buf.silenceFrames-voiceUtteranceEOTGraceFrames)%voiceUtteranceEOTCheckIntervalFrames == 0 {
					// eotTrailingRawSamples' own comment has the real bug
					// this closes — resample only the trailing window the
					// model actually needs, not the whole utterance so far.
					window := buf.pcm
					if len(window) > eotTrailingRawSamples {
						window = window[len(window)-eotTrailingRawSamples:]
					}
					mono16k := downmixResample16kMonoInt16(window)
					probability, eotErr := eot.predict(context.Background(), mono16k)
					if eotErr != nil {
						if !buf.eotFailureLogged {
							log.Printf("discord-voice: EOT sidecar call failed, falling back to fixed timeout: %v", eotErr)
							buf.eotFailureLogged = true
						}
					} else if probability >= eotThreshold {
						ended = true
					}
				}
				if ended {
					flush(pkt.SSRC, buf)
				}

			default:
				// Silent, and never confirmed — that speech-classified run,
				// if there was one, wasn't sustained enough to be real.
				// Discard it rather than let it linger and get spliced onto
				// a later, unrelated speech burst.
				buf.speechFrames = 0
				buf.pendingSpeechPCM = nil
			}
			mu.Unlock()
		}
	}
}
