package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/worker"
)

// discordConnectionLeaseTTL is a real starting value, not claimed to be the
// tuned-right one — same numeric-tuning discipline as every other
// undecided-but-must-ship-with-something interval in this project.
const discordConnectionLeaseTTL = 30 * time.Second

// discordReplyChainMaxDepth bounds resolveDiscordThreadRoot's walk so a
// pathological reply chain can't become an unbounded query
// (gateway/discord.md's own "Reply-chain walk depth bound" open item). A
// real value given directly, since an unbounded walk is a genuine risk, not
// deferred the way a pure UX-tuning number would be — not claimed to be the
// "right" number either.
const discordReplyChainMaxDepth = 50

// startDiscordPlatform runs the Discord goroutine for this tenant's Gateway
// process — docs/components/gateway.md's "one goroutine per platform kind
// that tenant has actually configured" per-tenant deployment model.
//
// connectionID (gateway.md's "Resolved: Outbound Flow", 2026-08-25
// correction) is resolved ONCE here, before the lease loop even starts — a
// plain REST call (GET /users/@me), no live gateway/websocket connection
// needed — since the lease itself is keyed on it (leases.go). The same
// discordgo.Session is reused across every reconnect attempt rather than
// rebuilt each time, since its REST client (and thus connectionID) doesn't
// need re-resolving on a reconnect.
func (s *server) startDiscordPlatform(ctx context.Context, botToken, holderID string) {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Printf("discord: failed to create session: %v", err)
		return
	}
	// IntentsGuildVoiceStates: required for discordgo's own state tracker to
	// know which voice channel a user is currently in (dg.State.VoiceState,
	// discord_voice.go's voiceJoin) — without it, /join can never resolve
	// who to follow into a channel.
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent |
		discordgo.IntentsDirectMessages | discordgo.IntentsGuildVoiceStates

	botUser, err := dg.User("@me")
	if err != nil {
		log.Printf("discord: failed to resolve bot identity (GET /users/@me): %v", err)
		return
	}
	connectionID := botUser.ID
	dg.AddHandler(func(session *discordgo.Session, m *discordgo.MessageCreate) {
		s.discordMessageCreate(session, m, connectionID)
	})
	dg.AddHandler(func(session *discordgo.Session, ic *discordgo.InteractionCreate) {
		// ctx (this function's own parameter, closed over here) is the
		// process's real shutdown-tied context — voiceJoin derives its
		// long-running goroutines' lifetime from it, not from
		// context.Background(), so a voice connection actually gets torn
		// down on SIGTERM instead of being orphaned.
		//
		// Two interaction categories today, dispatched by type: slash
		// commands (/join, /leave — voice) and message components (buttons
		// on a mid-turn user-input prompt — docs/components/user-input.md,
		// "Resolved: Response-Routing for Discord Text"). A future third
		// category adds a new dispatch case here, not a second AddHandler
		// call — discordgo delivers every interaction to every registered
		// handler, so multiple handlers would each need their own type
		// filter.
		switch ic.Type {
		case discordgo.InteractionApplicationCommand:
			// /join and /leave are voice-connection commands; /mode is the
			// text-channel/DM reply-mode toggle (discord_reply_mode.go).
			if ic.ApplicationCommandData().Name == "mode" {
				s.handleModeCommand(ctx, session, ic)
			} else {
				s.discordVoiceInteractionCreate(ctx, session, ic, connectionID)
			}
		case discordgo.InteractionMessageComponent:
			s.discordUserInputInteractionCreate(ctx, session, ic, connectionID)
		}
	})
	// connectionID (botUser.ID, resolved above via REST) doubles as the
	// application id here — true for a standard single-application bot
	// token, and avoids depending on dg.State.User being populated, which
	// only happens after dg.Open() (called later, inside
	// runDiscordConnection) — registerVoiceCommands needs to run before
	// that dependency would even be satisfiable if it read state instead.
	s.registerVoiceCommands(dg, connectionID)

	for {
		ok, err := s.acquireOrRenewConnectionLease(ctx, "discord", connectionID, holderID, discordConnectionLeaseTTL)
		if err != nil {
			log.Printf("discord: lease acquire error: %v", err)
		}
		if ok {
			s.runDiscordConnection(ctx, dg, connectionID, holderID)
			// runDiscordConnection blocks until the connection drops or the
			// lease is lost; on return, loop back and retry acquisition.
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(discordConnectionLeaseTTL / 3):
		}
	}
}

// runDiscordConnection holds one connection cycle: opens the live socket,
// starts this connection's own embedded Temporal worker (registered for
// "DiscordDeliver" on deliver:discord:{connectionID} — gateway.md's
// "Resolved: Outbound Flow" — only for as long as this replica actually
// holds the lease, since the worker closes over this specific *discordgo.Session*
// and a lease loss means that session is about to be closed too), and renews
// the lease on a ticker until either the lease is lost or ctx is cancelled.
func (s *server) runDiscordConnection(ctx context.Context, dg *discordgo.Session, connectionID, holderID string) {
	if err := dg.Open(); err != nil {
		log.Printf("discord: failed to open connection: %v", err)
		return
	}
	defer dg.Close()
	defer s.releaseConnectionLease(context.Background(), "discord", connectionID, holderID)

	log.Printf("discord: connected as %s, holding connection lease as %s", connectionID, holderID)

	deliverActivity := &discordDeliverActivity{session: dg, pool: s.pool, connectionID: connectionID}
	// DisableWorkflowWorker: this queue only ever serves DiscordDeliver — no
	// workflow is ever dispatched to it, so there's nothing for a workflow
	// poller to do here.
	deliverWorker := worker.New(s.temporal, "deliver:discord:"+connectionID, worker.Options{DisableWorkflowWorker: true})
	deliverWorker.RegisterActivityWithOptions(deliverActivity.Deliver, activity.RegisterOptions{Name: "DiscordDeliver"})
	// docs/components/gateway.md's "Resolved: ModelCall Streaming" —
	// same worker/connection, same live session, registered alongside
	// DiscordDeliver rather than a separate embedded worker.
	deliverWorker.RegisterActivityWithOptions(deliverActivity.DeliverChunk, activity.RegisterOptions{Name: "DiscordDeliverChunk"})
	// docs/components/user-input.md's "Mid-turn interim delivery" (push
	// half, A+B) — same embedded worker, same connection, since pushing a
	// pending request's prompt needs the identical live session.
	deliverWorker.RegisterActivityWithOptions(deliverActivity.DeliverInterim, activity.RegisterOptions{Name: "DiscordDeliverInterim"})
	if err := deliverWorker.Start(); err != nil {
		log.Printf("discord: failed to start embedded delivery worker: %v", err)
		return
	}
	defer deliverWorker.Stop()

	ticker := time.NewTicker(discordConnectionLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.acquireOrRenewConnectionLease(ctx, "discord", connectionID, holderID, discordConnectionLeaseTTL)
			if err != nil {
				log.Printf("discord: lease renew error: %v", err)
				continue
			}
			if !ok {
				log.Printf("discord: lost connection lease, disconnecting")
				return
			}
		}
	}
}

// discordMessageCreate — gateway/discord.md's "Resolved: Response Scope" and
// "Resolved: Ambient Message Buffer". Every real (non-bot) message gets
// ingested into discord_ambient_messages unconditionally; only a message
// that @mentions this bot or replies to one of this bot's own messages goes
// on to actually submit a MessageEvent (a real turn). connectionID is this
// specific bot's own resolved identity (startDiscordPlatform), closed over
// per-bot rather than read from shared mutable state — multi-bot-ready
// without any locking, since each bot's own goroutine/handler closure
// carries its own value independently.
func (s *server) discordMessageCreate(session *discordgo.Session, m *discordgo.MessageCreate, connectionID string) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	// gateway/discord.md's "Non-content MessageCreate events aren't filtered
	// by type" gap: MessageTypeDefault (a plain post) and MessageTypeReply
	// are the only two types that represent genuine human conversational
	// content. Everything else Discord delivers as a MessageCreate too — a
	// channel-follow-add, a guild-boost announcement, an "app added, here's
	// how to get started" card, a call-start notification, a thread-created
	// system message — is Discord-generated, not a human saying something,
	// and must never reach discord_ambient_messages or trigger a turn. Real
	// and observed in a personal DM specifically, where every message is
	// already an implicit trigger with no mention/reply gate to catch this
	// the way a guild channel incidentally would. Checked first, before the
	// ambient insert, so a filtered event leaves no trace at all.
	if m.Type != discordgo.MessageTypeDefault && m.Type != discordgo.MessageTypeReply {
		return
	}
	ctx := context.Background()

	var replyTo *string
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		id := m.MessageReference.MessageID
		replyTo = &id
	}

	// Whether this message is addressed to the bot — computed BEFORE the
	// voice-message transcription below (2026-08-30) so that a transcription
	// failure on a message that WAS a real trigger can be reported back to
	// the user instead of vanishing with only a server-side log.
	// gateway/discord.md's "Resolved: DMs" — a personal (1:1) DM has no
	// "other people talking" ambiguity for the mention/reply gate to resolve
	// (the whole reason that gate exists — see "Resolved: Response Scope"),
	// so every message there is an implicit trigger. A GROUP DM still
	// requires an explicit mention/reply, same as a guild channel.
	// m.GuildID alone can't distinguish a personal DM from a group DM (both
	// are empty), hence the extra isPersonalDM check.
	botUser := session.State.User
	mentioned := botUser != nil && discordMentionsUser(m.Mentions, botUser.ID)
	repliesToBot := botUser != nil && replyTo != nil &&
		m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil &&
		m.ReferencedMessage.Author.ID == botUser.ID
	personalDM := m.GuildID == "" && isPersonalDM(session, m.ChannelID)
	isTrigger := mentioned || repliesToBot || personalDM

	// gateway/discord.md's "Attachments/stickers/audio as real triggering
	// content" plan, the voice-message third of it: Discord's own
	// voice-message feature always sends an empty m.Content — the real
	// content only exists as the attached Ogg/Opus clip — so resolve it to a
	// real transcript here, before anything downstream (ambient buffer,
	// MessageEvent) looks at "content".
	content := m.Content
	if m.Flags&discordgo.MessageFlagsIsVoiceMessage != 0 {
		transcript, err := discordVoiceMessageContent(ctx, m)
		if err != nil {
			// No usable content to fall back to (unlike a plain
			// attachment/sticker, spoken audio that failed to transcribe has
			// no meaningful placeholder). Previously a silent drop — now, if
			// the message was actually addressed to the bot, say so, so the
			// user isn't left waiting on a reply that will never come.
			log.Printf("discord: failed to transcribe voice message %s (channel %s): %v", m.ID, m.ChannelID, err)
			if isTrigger {
				s.sendPlainDiscordReply(session, m.ChannelID, "Sorry — I couldn't make out that voice message. Mind trying again or typing it?")
			}
			return
		}
		// Same downstream filtering discord_voice.go's own flush applies to a
		// live utterance transcript: an empty transcript (silence, nothing
		// intelligible) or a pure vocal filler ("um"/"uh", voiceFillerWords)
		// carries no real content. isBackchannelOnly is deliberately NOT
		// applied here — it's gated on the utterance having started while the
		// bot was speaking on a live voice connection, a context that doesn't
		// exist for a voice note dropped into a text channel.
		if transcript == "" {
			log.Printf("discord: voice message %s transcribed to empty text", m.ID)
			if isTrigger {
				s.sendPlainDiscordReply(session, m.ChannelID, "I didn't catch anything in that voice message — try again?")
			}
			return
		}
		if isFillerOnly(transcript) {
			log.Printf("discord: filtered filler-only voice message transcript %q, no message recorded", transcript)
			return
		}
		content = transcript
	}

	if _, err := s.pool.Exec(ctx,
		"INSERT INTO discord_ambient_messages (channel_id, platform_message_id, reply_to_platform_message_id, author, content) "+
			"VALUES ($1, $2, $3, $4, $5) ON CONFLICT (channel_id, platform_message_id) DO NOTHING",
		m.ChannelID, m.ID, replyTo, m.Author.ID, content,
	); err != nil {
		log.Printf("discord: failed to record ambient message: %v", err)
		return
	}

	if !isTrigger {
		return
	}

	// Acknowledge receipt immediately with a 👀 reaction on the triggering
	// message — real turn processing (below) can take a while (a live model
	// call, possibly tool use), and Discord gives no other built-in
	// "received" signal the way Web's own UI can just show a spinner. Fired
	// in its own goroutine, not awaited: this is a pure UX nicety, not
	// something the actual message-processing path should ever wait on or
	// fail because of. Best-effort — a failed reaction (rate limit, missing
	// permission) just means no visual ack, never a dropped message.
	go func() {
		if err := session.MessageReactionAdd(m.ChannelID, m.ID, "👀"); err != nil {
			log.Printf("discord: failed to add ack reaction: %v", err)
		}
	}()

	// gateway.md's "Resolved: Multi-Session Channels" — root-collapse
	// discriminator policy (see gateway/discord.md). A plain mention with no
	// reply stays on the channel's main session; a reply walks to the reply
	// chain's root. If the chain can't be resolved (first hop not in the
	// buffer), falls back to the main session too, same as no reply at all.
	discriminator := "channel:" + m.ChannelID
	parentSessionKey := ""
	if replyTo != nil {
		rootID, err := s.resolveDiscordThreadRoot(ctx, m.ChannelID, *replyTo)
		if err != nil {
			log.Printf("discord: failed to resolve reply chain root: %v", err)
		} else if rootID != "" {
			discriminator = "reply_to_platform_message_id:" + rootID
			parentSessionKey = sessionKeyFor("discord", m.ChannelID, "channel:"+m.ChannelID)
		}
	}

	event := MessageEvent{
		Platform:          "discord",
		ChannelID:         m.ChannelID,
		User:              m.Author.ID,
		Content:           content,
		PlatformMessageID: m.ID,
		Discriminator:     discriminator,
		ParentSessionKey:  parentSessionKey,
		ConnectionID:      connectionID,
	}
	if _, err := s.submitMessageEvent(ctx, event); err != nil {
		log.Printf("discord: failed to submit message event: %v", err)
	}
}

func discordMentionsUser(mentions []*discordgo.User, userID string) bool {
	for _, u := range mentions {
		if u != nil && u.ID == userID {
			return true
		}
	}
	return false
}

// isPersonalDM reports whether channelID is a 1:1 DM (discordgo.ChannelTypeDM),
// as opposed to a group DM (discordgo.ChannelTypeGroupDM) — both have an
// empty GuildID on a message, so that alone doesn't distinguish them. Checks
// the local state cache first (discordgo maintains it automatically as
// channels are observed over the gateway connection) before falling back to
// a REST call for a channel not yet seen this way.
func isPersonalDM(session *discordgo.Session, channelID string) bool {
	if ch, err := session.State.Channel(channelID); err == nil {
		return ch.Type == discordgo.ChannelTypeDM
	}
	ch, err := session.Channel(channelID)
	if err != nil {
		// Can't determine — default to requiring an explicit mention/reply
		// instead (the safer failure mode: under-triggering, not an
		// unexpected reply in a channel that turns out to be a group DM).
		return false
	}
	return ch.Type == discordgo.ChannelTypeDM
}

// resolveDiscordThreadRoot walks reply_to_platform_message_id backward
// through discord_ambient_messages until reaching a message with no reply
// reference of its own — gateway/discord.md's root-collapse discriminator
// policy: every reply anywhere in one chain converges on the same root, so
// the whole chain stays one coherent session rather than fragmenting.
// Returns "" (not an error) if the chain can't be resolved at all — the
// first hop isn't in the buffer (aged out, pruned, or predates the bot
// joining the channel) — the caller falls back to the main channel session
// in that case, same as a plain mention with no reply.
//
// Note: for a reply-to-the-bot's-own-earlier-reply to resolve correctly past
// this bot's own messages, the bot's own outbound sends would need to land
// in discord_ambient_messages too (with their own
// reply_to_platform_message_id when the bot itself was replying to
// something) — DiscordDeliver (deliver_discord.go) does not do this yet;
// still a real, open gap now that outbound delivery is actually built.
func (s *server) resolveDiscordThreadRoot(ctx context.Context, channelID, startMessageID string) (string, error) {
	current := startMessageID
	for i := 0; i < discordReplyChainMaxDepth; i++ {
		var replyTo *string
		err := s.pool.QueryRow(ctx,
			"SELECT reply_to_platform_message_id FROM discord_ambient_messages WHERE channel_id = $1 AND platform_message_id = $2",
			channelID, current,
		).Scan(&replyTo)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", nil
			}
			return "", err
		}
		if replyTo == nil {
			return current, nil
		}
		current = *replyTo
	}
	// Depth bound hit — treat the current position as the root rather than
	// erroring; an unusually long chain still gets a stable, real session,
	// just possibly not the structurally true root.
	return current, nil
}
