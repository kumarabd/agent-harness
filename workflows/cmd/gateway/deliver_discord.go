package main

import (
	"context"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5/pgxpool"
)

// discordDeliverActivity is the real implementation of docs/components/
// gateway.md's "Resolved: Outbound Flow" DeliverActivity, for Discord
// specifically. Registered as "DiscordDeliver" on this connection's own
// embedded Temporal worker (see discord.go's runDiscordConnection) — created
// fresh, and only registered, for exactly as long as this replica holds the
// connection's lease, since session (the live discordgo socket) is only
// valid for that long.
type discordDeliverActivity struct {
	session      *discordgo.Session
	pool         *pgxpool.Pool
	connectionID string
}

// Deliver reads the turn's final assistant message, checks-then-inserts into
// the delivered_responses idempotency ledger (components/
// activities-outbound-delivery.md's "Resolved: Deliver Idempotency Key" —
// response_id = turn_id, unchanged by any of this), and — only on a genuine
// first delivery — sends it to the Discord channel that turn's session
// belongs to. turn_id -> session_key -> channel_id resolves via one join
// (turns.parent_id IS the session_key for a top-level turn, the only kind
// this is ever called for — turn.go only dispatches this when
// input.ParentType == "session").
func (a *discordDeliverActivity) Deliver(ctx context.Context, turnID string) error {
	tag, err := a.pool.Exec(ctx,
		"INSERT INTO delivered_responses (response_id) VALUES ($1) ON CONFLICT DO NOTHING",
		turnID,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Already delivered by an earlier attempt at this exact turn_id —
		// same at-least-once-dispatch-to-effectively-once-visible-action
		// reasoning activities-outbound-delivery.md's own idempotency
		// section describes, just enforced here instead of a broker.
		return nil
	}

	var channelID, content string
	err = a.pool.QueryRow(ctx, `
		SELECT s.channel_id, m.content
		FROM turns t
		JOIN sessions s ON s.session_key = t.parent_id
		JOIN LATERAL (
			SELECT content FROM messages
			WHERE parent_id = t.turn_id AND role = 'assistant'
			ORDER BY seq DESC LIMIT 1
		) m ON true
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &content)
	if err != nil {
		return err
	}
	if content == "" {
		// docs/future-work.md §4 — a real, separately-tracked gap (the model
		// sometimes ends a turn with no real content). Nothing to send;
		// not this activity's job to paper over it.
		return nil
	}

	if _, err := a.session.ChannelMessageSend(channelID, content); err != nil {
		return err
	}
	log.Printf("discord: delivered turn %s to channel %s via connection %s", turnID, channelID, a.connectionID)
	return nil
}
