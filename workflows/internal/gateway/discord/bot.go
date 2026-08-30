// Package discord is the Discord text-channel/DM gateway platform: it holds
// the single leased *discordgo.Session, ingests messages (ambient buffer +
// mention/reply/DM-triggered turns), runs the DiscordDeliver* Temporal
// activities on an embedded per-connection worker, and handles slash
// commands and user-input buttons.
//
// It owns the Discord connection outright; the discordvoice platform is a
// child it constructs and delegates /join, /leave, and voice interactions
// to (a voice connection is negotiated as a child of this open text
// session).
package discord

import (
	"context"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/discordvoice"
	"agent-harness/workflows/internal/gateway/lease"
)

// Bot is one Discord bot connection. One per token in DISCORD_BOT_TOKENS.
type Bot struct {
	token    string
	ingestor *core.Ingestor
	pool     *pgxpool.Pool
	temporal client.Client
	leases   *lease.Manager
	voice    *discordvoice.Voice
}

// New wires a Bot (and its child discordvoice.Voice) for one bot token.
func New(ctx context.Context, token string, ingestor *core.Ingestor, pool *pgxpool.Pool, temporal client.Client, leases *lease.Manager) *Bot {
	return &Bot{
		token:    token,
		ingestor: ingestor,
		pool:     pool,
		temporal: temporal,
		leases:   leases,
		voice:    discordvoice.New(ctx, ingestor, pool, temporal, leases),
	}
}

// registerCommands creates every global slash command this bot answers —
// discordvoice's /join and /leave plus this package's /mode — in one place.
// appID is the bot's own resolved user id (works for a standard
// single-application bot token) and avoids depending on dg.State.User, which
// isn't populated until after dg.Open().
func registerCommands(dg *discordgo.Session, appID string) {
	commands := discordvoice.Commands()
	// docs/components/gateway/discord.md's "Resolved: Per-Channel Reply Mode".
	commands = append(commands, &discordgo.ApplicationCommand{
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
	})
	for _, cmd := range commands {
		if _, err := dg.ApplicationCommandCreate(appID, "", cmd); err != nil {
			log.Printf("discord: failed to register /%s command: %v", cmd.Name, err)
		}
	}
}
