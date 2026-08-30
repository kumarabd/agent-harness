package main

import (
	"context"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// docs/components/gateway/discord.md's "Resolved: Per-Channel Reply Mode"
// (2026-08-30). `/mode reply:voice|text` switches a Discord text channel or
// DM between a written reply and a spoken (voice-message) one. Stored per
// channel_id in discord_reply_mode (012_discord_reply_mode.sql); the
// DiscordDeliver / DiscordDeliverChunk activities read it at delivery time.
// Audio the human sends is transcribed and answered regardless of the mode —
// this only controls what the bot's OWN reply looks like.

const (
	replyModeText  = "text"
	replyModeVoice = "voice"
)

// discordReplyMode returns the channel's configured reply mode, defaulting
// to "text" whenever there is no row or the read errors — failing to the
// current, always-safe behavior (a text send always works; a voice send has
// TTS + a raw multipart upload that can each fail).
func discordReplyMode(ctx context.Context, pool *pgxpool.Pool, channelID string) string {
	var mode string
	if err := pool.QueryRow(ctx,
		"SELECT mode FROM discord_reply_mode WHERE channel_id = $1", channelID,
	).Scan(&mode); err != nil {
		return replyModeText
	}
	return mode
}

// handleModeCommand processes the /mode slash command (registered in
// registerDiscordCommands, routed from discord.go's interaction dispatch).
func (s *server) handleModeCommand(ctx context.Context, dg *discordgo.Session, ic *discordgo.InteractionCreate) {
	opts := ic.ApplicationCommandData().Options
	if len(opts) == 0 {
		s.respondInteraction(dg, ic, "Usage: `/mode reply:voice` or `/mode reply:text`")
		return
	}
	mode := opts[0].StringValue()
	if mode != replyModeText && mode != replyModeVoice {
		s.respondInteraction(dg, ic, "Reply mode must be `voice` or `text`.")
		return
	}

	channelID := ic.ChannelID
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO discord_reply_mode (channel_id, mode) VALUES ($1, $2)
		 ON CONFLICT (channel_id) DO UPDATE SET mode = EXCLUDED.mode, updated_at = now()`,
		channelID, mode,
	); err != nil {
		log.Printf("discord: failed to set reply mode for channel %s: %v", channelID, err)
		s.respondInteraction(dg, ic, "Couldn't save that — try again in a moment.")
		return
	}

	if mode == replyModeVoice {
		s.respondInteraction(dg, ic, "Reply mode set to **voice** — I'll answer with voice messages here. Anything you send, text or audio, is understood the same way.")
	} else {
		s.respondInteraction(dg, ic, "Reply mode set to **text** for this channel.")
	}
	log.Printf("discord: reply mode for channel %s set to %s", channelID, mode)
}

// respondInteraction sends a plain immediate response to a slash-command
// interaction. Shared by handleModeCommand; the voice /join,/leave handler
// (discord_voice.go) has its own equivalent inline and is left untouched.
func (s *server) respondInteraction(dg *discordgo.Session, ic *discordgo.InteractionCreate, content string) {
	if err := dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Content: content},
	}); err != nil {
		log.Printf("discord: failed to respond to /mode interaction: %v", err)
	}
}

// sendPlainDiscordReply posts a short plain-text message to a channel,
// best-effort (a failed send just logs). Used for the "I couldn't transcribe
// that voice message" nudge in discordMessageCreate — always text, never a
// voice message, regardless of the channel's reply mode: it's an operational
// notice, not an answer to a turn.
func (s *server) sendPlainDiscordReply(dg *discordgo.Session, channelID, content string) {
	if _, err := dg.ChannelMessageSend(channelID, content); err != nil {
		log.Printf("discord: failed to send plain reply to channel %s: %v", channelID, err)
	}
}
