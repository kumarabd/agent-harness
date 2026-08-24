package main

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
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
// that tenant has actually configured" per-tenant deployment model. Connects
// only while holding this platform's connection lease
// (gateway_connection_leases, leases.go) — retries acquisition on a fixed
// interval otherwise, never blocking, matching leases.go's own
// never-blocks contract.
func (s *server) startDiscordPlatform(ctx context.Context, botToken, holderID string) {
	for {
		ok, err := s.acquireOrRenewConnectionLease(ctx, "discord", holderID, discordConnectionLeaseTTL)
		if err != nil {
			log.Printf("discord: lease acquire error: %v", err)
		}
		if ok {
			s.runDiscordConnection(ctx, botToken, holderID)
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

func (s *server) runDiscordConnection(ctx context.Context, botToken, holderID string) {
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Printf("discord: failed to create session: %v", err)
		return
	}
	dg.Identify.Intents = discordgo.IntentsGuildMessages | discordgo.IntentsMessageContent | discordgo.IntentsDirectMessages
	dg.AddHandler(s.discordMessageCreate)

	if err := dg.Open(); err != nil {
		log.Printf("discord: failed to open connection: %v", err)
		return
	}
	defer dg.Close()
	defer s.releaseConnectionLease(context.Background(), "discord", holderID)

	log.Printf("discord: connected, holding connection lease as %s", holderID)

	ticker := time.NewTicker(discordConnectionLeaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			ok, err := s.acquireOrRenewConnectionLease(ctx, "discord", holderID, discordConnectionLeaseTTL)
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
// on to actually submit a MessageEvent (a real turn).
func (s *server) discordMessageCreate(session *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot {
		return
	}
	ctx := context.Background()

	var replyTo *string
	if m.MessageReference != nil && m.MessageReference.MessageID != "" {
		id := m.MessageReference.MessageID
		replyTo = &id
	}

	if _, err := s.pool.Exec(ctx,
		"INSERT INTO discord_ambient_messages (channel_id, platform_message_id, reply_to_platform_message_id, author, content) "+
			"VALUES ($1, $2, $3, $4, $5) ON CONFLICT (channel_id, platform_message_id) DO NOTHING",
		m.ChannelID, m.ID, replyTo, m.Author.ID, m.Content,
	); err != nil {
		log.Printf("discord: failed to record ambient message: %v", err)
		return
	}

	botUser := session.State.User
	mentioned := botUser != nil && discordMentionsUser(m.Mentions, botUser.ID)
	repliesToBot := botUser != nil && replyTo != nil &&
		m.ReferencedMessage != nil && m.ReferencedMessage.Author != nil &&
		m.ReferencedMessage.Author.ID == botUser.ID

	if !mentioned && !repliesToBot {
		return
	}

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
		Content:           m.Content,
		PlatformMessageID: m.ID,
		Discriminator:     discriminator,
		ParentSessionKey:  parentSessionKey,
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
// Note (not yet a real limitation, since Discord's own outbound Deliver
// mechanism isn't built yet): for a reply-to-the-bot's-own-earlier-reply to
// resolve correctly past this bot's own messages, the bot's own outbound
// sends need to land in discord_ambient_messages too (with their own
// reply_to_platform_message_id when the bot itself was replying to
// something) — not needed for this pass since nothing sends outbound yet,
// but a real requirement for whichever pass builds DeliverActivity for
// Discord.
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
