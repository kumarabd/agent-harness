package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// buildUserInputComponents turns a user_input_requests row's options into
// Discord message components (buttons), one row of up to 5 buttons each,
// max 5 rows (Discord's own limits). custom_id encodes the routing —
// discord_user_input.go's interaction handler parses it back apart on
// click. Deliberately hard-caps at 25 options: beyond that, a select-menu
// component would be the right shape, but no consumer today generates
// more than a handful of options, so accepting the cap now rather than
// adding an unused fallback path.
//
// The custom_id format ("user_input:<request_id>:<option_id>") stays
// under Discord's 100-char custom_id ceiling for realistic values —
// request_id is a UUID (36 chars) and option_id is a short string
// ("approve", "deny") in the only consumer that exists today (permission
// gating). If a future consumer introduces longer option ids, the button
// build call will still succeed but the click handler will reject
// oversized ids with a clean error — no silent truncation.
func buildUserInputComponents(requestID string, options []struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}) []discordgo.MessageComponent {
	const (
		maxButtonsPerRow = 5
		maxRows          = 5
		maxOptions       = maxButtonsPerRow * maxRows
	)
	if len(options) > maxOptions {
		options = options[:maxOptions]
	}
	var rows []discordgo.MessageComponent
	for i := 0; i < len(options); i += maxButtonsPerRow {
		end := i + maxButtonsPerRow
		if end > len(options) {
			end = len(options)
		}
		var buttons []discordgo.MessageComponent
		for _, opt := range options[i:end] {
			buttons = append(buttons, discordgo.Button{
				Label: opt.Label,
				Style: discordgo.PrimaryButton,
				// Prefix scopes this custom_id to the user-input feature —
				// discord_user_input.go's handler filters on this exact
				// prefix so we never accidentally handle a click for a
				// future unrelated button type. Colons are safe because
				// request_id (UUID) and option_id ("approve"/"deny"/...)
				// don't contain them in any consumer today.
				CustomID: "user_input:" + requestID + ":" + opt.ID,
			})
		}
		rows = append(rows, discordgo.ActionsRow{Components: buttons})
	}
	return rows
}

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

// discordEmptyContentPlaceholder / discordSendableContent — real, live bug
// found 2026-08-29, revised the same day per direct feedback: Discord's own
// API hard-rejects a message whose content is empty or pure whitespace
// (HTTP 400, "Cannot send an empty message" — confirmed live, not assumed;
// this is what wedged a real turn's streamed delivery in the first place,
// since the resulting failure had no retry cap). The FIRST fix silently
// skipped sending in that case (mark delivered/sent, no message) — reverted
// after being asked directly whether that's actually correct: whitespace can
// be a genuine, meaningful stream chunk, and other platforms may not share
// Discord's own rejection at all, so treating "empty" as "nothing to do" is
// the wrong generalization to bake into a shared path. This is Discord's own
// platform constraint, so it's handled here, at the Discord-specific
// outbound boundary, not upstream in turn.go or model_call.py — the model's
// real content in messages/turn_deliveries is never touched by this, only
// what actually gets handed to Discord's API. A visible placeholder, not a
// silent no-op: the model's turn should never look like nothing happened.
const discordEmptyContentPlaceholder = "(empty)"

// discordSendableContent returns content unchanged unless it's empty or
// pure whitespace, in which case it substitutes discordEmptyContentPlaceholder
// — called only immediately before a real ChannelMessageSend/ChannelMessageEdit
// call, never used to decide whether to skip one.
func discordSendableContent(content string) string {
	if strings.TrimSpace(content) == "" {
		return discordEmptyContentPlaceholder
	}
	return content
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

	var channelID, sessionKey, content string
	var streamedMessageRef *string
	err := a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, m.content, t.streamed_message_ref
		FROM turns t
		JOIN sessions s ON s.session_key = t.parent_id
		JOIN LATERAL (
			SELECT content FROM messages
			WHERE parent_id = t.turn_id AND role = 'assistant'
			ORDER BY seq DESC LIMIT 1
		) m ON true
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &sessionKey, &content, &streamedMessageRef)
	if err != nil {
		return err
	}
	// docs/future-work.md §4 — a real, separately-tracked gap (the model
	// sometimes ends a turn with no real content). This activity no longer
	// papers over it by skipping the send entirely (see
	// discordSendableContent's own comment for why that changed) — content
	// stays the model's real raw value for every comparison/storage
	// purpose below; only the actual ChannelMessageSend call downstream
	// substitutes a visible placeholder if it's empty/whitespace.
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

	// gateway/discord.md's "Resolved: Per-Channel Reply Mode": if this
	// channel is set to voice, speak the answer as a native Discord voice
	// message. Any failure along the way (TTS, the raw multipart upload)
	// falls through to the normal text send below — the answer still gets
	// delivered, just written instead of spoken.
	if discordReplyMode(ctx, a.pool, channelID) == replyModeVoice {
		if msg, verr := a.deliverVoiceReply(ctx, channelID, content); verr == nil {
			a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, content)
			log.Printf("discord: delivered turn %s to channel %s as a voice message (%s)", turnID, channelID, msg.ID)
			return markDelivered()
		} else {
			log.Printf("discord: voice reply failed for turn %s, falling back to text: %v", turnID, verr)
		}
	}

	sendContent := discordSendableContent(content)
	msg, err := a.session.ChannelMessageSend(channelID, sendContent)
	if err != nil {
		return err
	}
	a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, sendContent)
	log.Printf("discord: delivered turn %s to channel %s via connection %s", turnID, channelID, a.connectionID)
	return markDelivered()
}

// deliverVoiceReply synthesizes a turn's answer to Ogg/Opus and posts it as
// a native Discord voice message. sanitizeForSpeech (voice_text_sanitize.go)
// strips emoji/markdown that a TTS engine would otherwise read out by name —
// the same backstop deliver_voice.go's live-voice path applies.
func (a *discordDeliverActivity) deliverVoiceReply(ctx context.Context, channelID, content string) (*discordgo.Message, error) {
	spoken := sanitizeForSpeech(content)
	if spoken == "" {
		return nil, errors.New("deliverVoiceReply: nothing to speak after sanitizing")
	}
	ogg, err := synthesizeSpeechOgg(ctx, spoken)
	if err != nil {
		return nil, err
	}
	if len(ogg) == 0 {
		return nil, errors.New("deliverVoiceReply: TTS returned no audio")
	}
	return sendDiscordVoiceMessage(a.session, channelID, ogg)
}

// recordAmbientBotMessage mirrors the bot's own sent/edited message into
// discord_ambient_messages — gateway/discord.md's "Discord-side reply-chain
// resolution past the bot's own messages" gap. Deliberately best-effort: the
// real Discord send has already succeeded by the time this is called, so a
// failure here must never turn into a retried (and therefore duplicated)
// send — log and move on, same tolerance discord.go's own best-effort
// logging elsewhere in this package uses, not the fail-the-whole-activity
// treatment a pre-send failure would warrant.
//
// ON CONFLICT DO UPDATE (not discordMessageCreate's own DO NOTHING) is
// deliberate: a human message's content is fixed the moment it's sent, but a
// streamed bot message's content genuinely changes across DeliverChunk's own
// edit-in-place calls — this keeps the ambient mirror's content current
// through every edit, not just the first chunk, while a message's
// reply_to_platform_message_id (derived once from its session's own root)
// never actually changes across those re-writes.
func (a *discordDeliverActivity) recordAmbientBotMessage(ctx context.Context, channelID, messageID, sessionKey, content string) {
	var replyTo *string
	if root := discordThreadRootFromSessionKey(sessionKey); root != "" {
		replyTo = &root
	}
	if _, err := a.pool.Exec(ctx,
		"INSERT INTO discord_ambient_messages (channel_id, platform_message_id, reply_to_platform_message_id, author, content) "+
			"VALUES ($1, $2, $3, $4, $5) "+
			"ON CONFLICT (channel_id, platform_message_id) DO UPDATE SET content = EXCLUDED.content",
		channelID, messageID, replyTo, a.connectionID, content,
	); err != nil {
		log.Printf("discord: failed to record ambient bot message %s: %v", messageID, err)
	}
}

// DeliverInterim pushes a pending user_input_requests row's prompt+options
// out to the Discord channel — docs/components/user-input.md's "Mid-turn
// interim delivery" (push half, A+B). Takes requestID, not turnID: unlike
// Deliver/DeliverChunk (which read a turn's own content), everything this
// needs — prompt, options, and (via one more join) the routing to a real
// channel_id — already lives on the user_input_requests row itself by the
// time UserInputRequestWorkflow dispatches this, right after RequestUserInput
// wrote it (reference-passing contract: the workflow hands over an ID, this
// activity reads the actual content).
//
// **Options render as real Discord buttons** (message components) —
// docs/components/user-input.md's response-routing resolution for Discord
// text (2026-08-28). Each button's custom_id is
// "user_input:{request_id}:{option_id}", which discord_user_input.go's
// interaction handler parses to route the click back as a
// UserInputResponse signal against the exact target workflow. Falls back
// to plain text (no components) for zero-options requests — a free-text-
// only request has nothing to render as a button, and Discord rejects an
// ActionRow with zero children anyway.
//
// Discord's own limits (verified against the API docs before writing):
// max 5 buttons per ActionRow, max 5 ActionRows per message, custom_id
// max 100 chars. We hard-cap options at 25 (5 rows × 5 each) — beyond
// that a UI-selectable list would be the right shape, but no consumer
// today generates more than a handful of options, so accepting the cap
// rather than adding an unused fallback path.
//
// Idempotency via prompt_delivered_at (008_user_input_interim_delivery.sql),
// deliberately separate from user_input_requests.status: "was the prompt
// pushed" and "has the human answered" are different questions — a Temporal
// retry of the dispatching ExecuteActivity call must not re-send the prompt
// a second time even though status is still 'pending'. Same
// check-before-send/mark-after-send-succeeds shape as Deliver's own fixed
// idempotency bug above — never claim delivery before the real send
// succeeds.
func (a *discordDeliverActivity) DeliverInterim(ctx context.Context, requestID string) error {
	var alreadyDelivered bool
	if err := a.pool.QueryRow(ctx,
		"SELECT prompt_delivered_at IS NOT NULL FROM user_input_requests WHERE request_id = $1", requestID,
	).Scan(&alreadyDelivered); err != nil {
		return err
	}
	if alreadyDelivered {
		return nil
	}

	var channelID, sessionKey, prompt string
	var optionsJSON []byte
	err := a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, r.prompt, r.options
		FROM user_input_requests r
		JOIN turns t ON t.turn_id = r.turn_id
		JOIN sessions s ON s.session_key = t.parent_id
		WHERE r.request_id = $1
	`, requestID).Scan(&channelID, &sessionKey, &prompt, &optionsJSON)
	if err != nil {
		return err
	}
	var options []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(optionsJSON, &options); err != nil {
		return err
	}

	send := &discordgo.MessageSend{Content: prompt}
	if len(options) > 0 {
		send.Components = buildUserInputComponents(requestID, options)
	}
	msg, err := a.session.ChannelMessageSendComplex(channelID, send)
	if err != nil {
		return err
	}
	// Same reasoning as Deliver's own recordAmbientBotMessage call: without
	// this, a later organic reply to this exact prompt message would hit
	// resolveDiscordThreadRoot's "bot message has no ambient row" gap all
	// over again (the fix built earlier this session), just for a different
	// kind of bot-sent message. Records the prompt text only (not the
	// options, which are now buttons rather than inline text) — the
	// options are Discord UI state, not conversational content.
	a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, prompt)

	if _, err := a.pool.Exec(ctx,
		"UPDATE user_input_requests SET prompt_delivered_at = now() WHERE request_id = $1", requestID,
	); err != nil {
		return err
	}
	log.Printf("discord: pushed pending request %s prompt to channel %s via connection %s", requestID, channelID, a.connectionID)
	return nil
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

	// Real, live bug found 2026-08-29: a streamed chunk's content can be
	// non-empty at the Python string level (segmenter flushed something)
	// while being pure whitespace once rendered — confirmed live, a real
	// turn's seq-1 chunk was 3 bytes of newlines. Discord's own API hard-
	// rejects that with a 400 ("Cannot send an empty message"), and since
	// this activity's caller (turn.go's deliverChunk) had no retry cap, the
	// failure retried forever, permanently wedging the whole turn workflow
	// before it ever reached ModelCall's already-completed result — not a
	// dropped preview, a genuinely stuck turn. VoiceDeliverChunk
	// (deliver_voice_chunk.go) already has an analogous guard, but its own
	// resolution (silently skip synthesis) is right for voice specifically
	// (nothing meaningful to speak for pure silence) and was NOT copied here
	// verbatim — revised same-day per direct feedback: a whitespace chunk
	// can be genuinely meaningful stream content, other platforms may not
	// even share this rejection, so silently skipping the send is the wrong
	// generalization for text. content stays the model's real raw value for
	// storage (turn_deliveries, the ambient mirror mostly cares about what's
	// actually visible though — see below); only the real
	// ChannelMessageSend/ChannelMessageEdit calls substitute a placeholder
	// via discordSendableContent, never used to decide whether to send.
	var channelID, sessionKey string
	var streamedMessageRef *string
	err = a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, t.streamed_message_ref
		FROM turns t JOIN sessions s ON s.session_key = t.parent_id
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &sessionKey, &streamedMessageRef)
	if err != nil {
		return err
	}

	// gateway/discord.md's "Resolved: Per-Channel Reply Mode": a voice-mode
	// channel gets ONE whole-turn voice message from Deliver, not a stream
	// of per-sentence text edits. Mark this chunk sent (so turn.go's
	// deliverChunk doesn't retry it) and leave streamed_message_ref unset,
	// so Deliver takes its normal non-streamed path and speaks the complete
	// final answer.
	if discordReplyMode(ctx, a.pool, channelID) == replyModeVoice {
		return markSent()
	}

	sendContent := discordSendableContent(content)

	if streamedMessageRef == nil {
		msg, err := a.session.ChannelMessageSend(channelID, sendContent)
		if err != nil {
			return err
		}
		if _, err := a.pool.Exec(ctx,
			"UPDATE turns SET streamed_message_ref = $1 WHERE turn_id = $2 AND streamed_message_ref IS NULL",
			msg.ID, turnID,
		); err != nil {
			return err
		}
		a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, sendContent)
		log.Printf("discord: turn %s streamed chunk %d created message %s", turnID, seq, msg.ID)
		return markSent()
	}

	if _, err := a.session.ChannelMessageEdit(channelID, *streamedMessageRef, sendContent); err != nil {
		return err
	}
	// Same message id as the create branch above — this call keeps the
	// ambient mirror's content current through the edit (ON CONFLICT DO
	// UPDATE, recordAmbientBotMessage's own comment), so a reply arriving
	// mid-stream still sees the latest text, not a stale first-chunk
	// snapshot. reply_to_platform_message_id is recomputed identically from
	// the same sessionKey each call — deterministic, so re-writing it here
	// is a no-op in practice, not a risk of drifting to a different value.
	a.recordAmbientBotMessage(ctx, channelID, *streamedMessageRef, sessionKey, sendContent)
	log.Printf("discord: turn %s streamed chunk %d edited message %s", turnID, seq, *streamedMessageRef)
	return markSent()
}
