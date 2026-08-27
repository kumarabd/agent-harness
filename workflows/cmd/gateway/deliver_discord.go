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
	// Real, live bug fixed 2026-08-26 (same pattern and same root cause as
	// deliver_voice.go's Deliver): this used to INSERT the idempotency row
	// here, BEFORE the real ChannelMessageSend — so a genuine failure
	// partway through (a transient Discord API error, a dropped connection)
	// still left the row committed, and Temporal's own automatic retry saw
	// "already delivered" and returned success without ever resending. A
	// read-only check here, with the real INSERT moved to every genuine-
	// completion return point below, means a retry after a real failure
	// correctly finds no row and resends, while a retry after real success
	// still correctly finds the row and skips it.
	var alreadyDelivered bool
	if err := a.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM delivered_responses WHERE response_id = $1)", turnID,
	).Scan(&alreadyDelivered); err != nil {
		return err
	}
	if alreadyDelivered {
		return nil
	}
	markDelivered := func() error {
		_, err := a.pool.Exec(ctx,
			"INSERT INTO delivered_responses (response_id) VALUES ($1) ON CONFLICT DO NOTHING", turnID)
		return err
	}

	var channelID, content string
	var streamedMessageRef *string
	err := a.pool.QueryRow(ctx, `
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
	if content == "" {
		// docs/future-work.md §4 — a real, separately-tracked gap (the model
		// sometimes ends a turn with no real content). Nothing to send;
		// not this activity's job to paper over it. Still a genuine
		// resolution, not a failure — mark delivered.
		return markDelivered()
	}
	if streamedMessageRef != nil {
		// docs/components/gateway.md's "Resolved: ModelCall Streaming" —
		// Real, live bug fixed 2026-08-27: streamedMessageRef only ever
		// means "this turn's first iteration was streamed" (turn.go's
		// streamingEligible gate is iterations==1, gated purely on
		// iteration count, not on whether the turn stopped there). This
		// used to skip unconditionally on the assumption that streaming a
		// turn's first content always means the whole eventual response
		// got streamed — true only when that first iteration made no tool
		// call. When it did, the turn kept going, and the real final
		// answer (a LATER, never-streamed `messages` row — confirmed live:
		// two real tool calls, a correct final answer sitting in Postgres,
		// and this activity still reporting success having sent nothing
		// else) was silently dropped forever, leaving the user staring at
		// the streamed "let me check..." remark with no follow-up.
		//
		// Comparing `content` (the turn's real latest assistant message)
		// against turn_deliveries' own last cumulative row (what the
		// streamed message actually shows right now) tells the two cases
		// apart: equal means iteration 1 WAS the whole turn and
		// DiscordDeliverChunk's last chunk already left the message
		// showing exactly this — nothing to add. Different means real,
		// later content exists that was never delivered — send it as a
		// follow-up message (not an edit: it's genuinely a separate
		// thought from a later iteration, not a growing revision of the
		// same one, matching how a human would post "checking... [pause]
		// here's what I found" as two messages, not one edited in place).
		var streamedContent string
		if err := a.pool.QueryRow(ctx,
			"SELECT COALESCE((SELECT content FROM turn_deliveries WHERE turn_id = $1 ORDER BY seq DESC LIMIT 1), '')",
			turnID,
		).Scan(&streamedContent); err != nil {
			return err
		}
		if streamedContent == content {
			log.Printf("discord: turn %s already delivered via streaming (message %s), skipping re-send", turnID, *streamedMessageRef)
			return markDelivered()
		}
	}

	if _, err := a.session.ChannelMessageSend(channelID, content); err != nil {
		return err
	}
	log.Printf("discord: delivered turn %s to channel %s via connection %s", turnID, channelID, a.connectionID)
	return markDelivered()
}

// DiscordDeliverChunk delivers one streamed sentence-chunk (docs/components/
// gateway.md's "Resolved: ModelCall Streaming") — creates the turn's
// message on the first chunk, edits it in place on every later one.
// Registered on the same embedded per-connection worker as Deliver above
// (discord.go's runDiscordConnection), since it needs the same live
// session.
func (a *discordDeliverActivity) DeliverChunk(ctx context.Context, turnID string, seq int) error {
	// Real, live bug fixed 2026-08-26 (same pattern as Deliver above): this
	// used to claim `sent = true` atomically in the same UPDATE that read
	// the content, BEFORE the real ChannelMessageSend/Edit call — so a
	// genuine failure partway through left the row already marked sent, and
	// a retry would see sent=true and skip resending. Read-only SELECT here
	// as the claim check; the real UPDATE moves to after the Discord API
	// call actually succeeds.
	var content string
	err := a.pool.QueryRow(ctx,
		"SELECT content FROM turn_deliveries WHERE turn_id = $1 AND seq = $2 AND sent = false",
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
	markSent := func() error {
		_, err := a.pool.Exec(ctx,
			"UPDATE turn_deliveries SET sent = true WHERE turn_id = $1 AND seq = $2", turnID, seq)
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
		return markSent()
	}

	if _, err := a.session.ChannelMessageEdit(channelID, *streamedMessageRef, content); err != nil {
		return err
	}
	log.Printf("discord: turn %s streamed chunk %d edited message %s", turnID, seq, *streamedMessageRef)
	return markSent()
}
