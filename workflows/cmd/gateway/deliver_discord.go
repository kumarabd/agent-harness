package main

import (
	"context"
	"errors"
	"log"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
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
	var streamedMessageRef *string
	err = a.pool.QueryRow(ctx, `
		SELECT s.channel_id, m.content, t.streamed_message_ref
		FROM turns t
		JOIN sessions s ON s.session_key = t.parent_id
		JOIN LATERAL (
			SELECT content FROM messages
			WHERE parent_id = t.turn_id AND role = 'assistant'
			ORDER BY seq DESC LIMIT 1
		) m ON true
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &content, &streamedMessageRef)
	if err != nil {
		return err
	}
	if streamedMessageRef != nil {
		// docs/components/gateway.md's "Resolved: ModelCall Streaming" —
		// this turn's response was already progressively delivered via
		// DiscordDeliverChunk's edit-in-place, and the last chunk's forced
		// final flush (llm.call_model_streaming's own docstring) already
		// left that message's content exactly matching the complete
		// response. Recording delivered_responses above (already done) is
		// enough to keep the ledger consistent — sending a second, whole
		// new message here would duplicate what the user already saw.
		log.Printf("discord: turn %s already delivered via streaming (message %s), skipping re-send", turnID, *streamedMessageRef)
		return nil
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

// DiscordDeliverChunk delivers one streamed sentence-chunk (docs/components/
// gateway.md's "Resolved: ModelCall Streaming") — creates the turn's
// message on the first chunk, edits it in place on every later one.
// Registered on the same embedded per-connection worker as Deliver above
// (discord.go's runDiscordConnection), since it needs the same live
// session.
func (a *discordDeliverActivity) DeliverChunk(ctx context.Context, turnID string, seq int) error {
	var content string
	err := a.pool.QueryRow(ctx,
		"UPDATE turn_deliveries SET sent = true WHERE turn_id = $1 AND seq = $2 AND sent = false RETURNING content",
		turnID, seq,
	).Scan(&content)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Already sent by an earlier attempt at this exact (turn_id,
			// seq) — same at-least-once-dispatch reasoning as Deliver's own
			// delivered_responses check above, enforced on this table
			// instead since a chunk and a final message are different
			// things with different identity shapes (gateway.md's own
			// migration comment).
			return nil
		}
		return err
	}

	var channelID string
	var streamedMessageRef *string
	err = a.pool.QueryRow(ctx, `
		SELECT s.channel_id, t.streamed_message_ref
		FROM turns t JOIN sessions s ON s.session_key = t.parent_id
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &streamedMessageRef)
	if err != nil {
		return err
	}

	if streamedMessageRef == nil {
		msg, err := a.session.ChannelMessageSend(channelID, content)
		if err != nil {
			return err
		}
		if _, err := a.pool.Exec(ctx,
			"UPDATE turns SET streamed_message_ref = $1 WHERE turn_id = $2 AND streamed_message_ref IS NULL",
			msg.ID, turnID,
		); err != nil {
			return err
		}
		log.Printf("discord: turn %s streamed chunk %d created message %s", turnID, seq, msg.ID)
		return nil
	}

	if _, err := a.session.ChannelMessageEdit(channelID, *streamedMessageRef, content); err != nil {
		return err
	}
	log.Printf("discord: turn %s streamed chunk %d edited message %s", turnID, seq, *streamedMessageRef)
	return nil
}
