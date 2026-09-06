package discord

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"unicode"

	"github.com/bwmarrin/discordgo"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/temporal"

	"agent-harness/workflows/internal/gateway/core"
	"agent-harness/workflows/internal/gateway/discordui"
	"agent-harness/workflows/internal/gateway/speech"
	"agent-harness/workflows/internal/types"
)


// discordDeliverActivity is the real implementation of docs/components/
// gateway.md's "Resolved: Outbound Flow" DeliverActivity, for Discord
// specifically. Registered as "DiscordDeliver" on this connection's own
// embedded Temporal worker (see discord.go's runConnection) — created
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

// discordMessageLengthLimit — real, live bug found 2026-09-06 (028_turns_
// streamed_message_offset.sql's own comment has the full story): Discord
// hard-rejects a message/edit over its per-message length cap (2000 chars
// standard, higher on boosted guilds). Deliberately conservative and fixed
// rather than detected per-guild — under-using a boosted guild's higher cap
// (an extra rollover message) is a far smaller cost than mis-detecting a
// boost level and hitting this exact 400 again.
const discordMessageLengthLimit = 1900

// discordSplitForLimit returns the largest prefix of runes that fits within
// limit, preferring to break at the last whitespace rune within that prefix
// (so a rollover doesn't cut a word in half) and falling back to a hard cut
// at exactly limit when no whitespace exists to break at (e.g. one giant
// unbroken token). Never returns an empty head when runes is non-empty —
// limit is always > 0 in every real call site here.
func discordSplitForLimit(runes []rune, limit int) (head, rest []rune) {
	if len(runes) <= limit {
		return runes, nil
	}
	cut := limit
	for i := limit - 1; i > 0; i-- {
		if unicode.IsSpace(runes[i]) {
			cut = i + 1 // keep the whitespace itself with the head, matching how it read before splitting
			break
		}
	}
	return runes[:cut], runes[cut:]
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

	// Real, live bug found 2026-09-06: a plan-owned turn (ParentType=="plan")
	// gets no automatic Deliver call from turn.go at all — plan_workflow.go's
	// runPlanPresentationTurn is expected to make its OWN delivery happen via
	// deliver_reply/deliver_attachment tool calls, but nothing enforces the
	// model actually calling one; a live run showed the model answering in
	// plain prose (content written to `messages` as always) with zero tool
	// calls, and since ParentType=="plan" skips the automatic path, that
	// content never reached Discord — total silence, turn marked completed.
	// plan_workflow.go now unconditionally calls Deliver(turnID) after that
	// turn regardless of what the model did, as a guaranteed-response safety
	// net (docs/components/activities-outbound-delivery.md's spirit: the
	// loop must ensure something reaches the user). This check makes that
	// safe to call unconditionally: if a deliver_reply/deliver_attachment
	// call already succeeded for this turn, the plan/answer already reached
	// the user via that tool — sending this turn's own separate `content`
	// on top would be a confusing duplicate, not a genuine second thing to
	// say, so skip the send (but still mark delivered, matching every other
	// genuine-completion path below).
	var alreadyDeliveredViaTool bool
	if err := a.pool.QueryRow(ctx,
		"SELECT EXISTS(SELECT 1 FROM tool_calls WHERE parent_id = $1 AND tool_name IN ('deliver_reply', 'deliver_attachment') AND status = 'ok')",
		turnID,
	).Scan(&alreadyDeliveredViaTool); err != nil {
		return err
	}
	if alreadyDeliveredViaTool {
		_, err := a.pool.Exec(ctx,
			"INSERT INTO delivered_responses (response_id) VALUES ($1) ON CONFLICT DO NOTHING", turnID)
		return err
	}
	markDelivered := func() error {
		_, err := a.pool.Exec(ctx,
			"INSERT INTO delivered_responses (response_id) VALUES ($1) ON CONFLICT DO NOTHING", turnID)
		return err
	}

	// Real, live bug found 2026-09-06: this join assumed t.parent_id IS the
	// session_key, true only for a top-level turn (parent_type='session').
	// Any turn under a PlanWorkflow — a checkpoint, a mid-plan follow-up, a
	// rejection notice, the plan-presentation turn — has parent_id pointing
	// at the PLAN (its planning turn's id), not the session, so this join
	// returned zero rows for every one of them, and the resulting error was
	// silently discarded at every call site (finishPlan/foldInFollowups/
	// plan_workflow.go's own `_ = ...Get(ctx, nil)` calls) — meaning no
	// checkpoint output or plan-owned turn's answer has ever actually
	// reached Discord through this path. Fixed generally: every plan-owned
	// turn's plan_id points at its planning turn, and the planning turn
	// itself is always top-level (parent_id IS the session_key, and it
	// self-references its own plan_id — confirmed live) — so joining through
	// COALESCE(t.plan_id, t.turn_id) resolves the right root for both a
	// plain top-level turn (plan_id NULL, root = itself) and any plan-owned
	// turn (root = its planning turn). Same fix applied to DeliverChunk and
	// DeliverReply/DeliverAttachment's deliverToolChannelAndPrompt below.
	var channelID, sessionKey, content string
	var streamedMessageRef *string
	err := a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, m.content, t.streamed_message_ref
		FROM turns t
		JOIN turns root ON root.turn_id = COALESCE(t.plan_id, t.turn_id)
		JOIN sessions s ON s.session_key = root.parent_id
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
	// Real, live bug found 2026-09-06: unlike DeliverChunk (streaming) and
	// DeliverInterim, this plain final-answer path had NO length guard at
	// all — a long non-streamed answer hit Discord's raw API rejection
	// (HTTP 400 BASE_TYPE_MAX_LENGTH) with no distinguishable error, and the
	// caller (turn.go's deliverConnectionBased) discards this activity's
	// error entirely, so the failure was completely invisible. Returning a
	// typed ContentTooLong error here (checked BEFORE the API round-trip,
	// not after) lets turn.go tell this apart from any other delivery
	// failure and run its model-driven recovery round (deliver_reply /
	// deliver_attachment) instead of just losing the response — see
	// deliverConnectionBased's own doc comment.
	if n := len([]rune(sendContent)); n > discordMessageLengthLimit {
		return temporal.NewApplicationErrorWithOptions(
			fmt.Sprintf("content too long for one Discord message: %d > %d", n, discordMessageLengthLimit),
			types.ErrTypeContentTooLong,
			temporal.ApplicationErrorOptions{NonRetryable: true, Details: []any{n, discordMessageLengthLimit}},
		)
	}
	msg, err := a.session.ChannelMessageSend(channelID, sendContent)
	if err != nil {
		return err
	}
	a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, sendContent)
	log.Printf("discord: delivered turn %s to channel %s via connection %s", turnID, channelID, a.connectionID)
	return markDelivered()
}

// deliverVoiceReply synthesizes a turn's answer to Ogg/Opus and posts it as
// a native Discord voice message. speech.SanitizeForSpeech (voice_text_sanitize.go)
// strips emoji/markdown that a TTS engine would otherwise read out by name —
// the same backstop deliver_voice.go's live-voice path applies.
func (a *discordDeliverActivity) deliverVoiceReply(ctx context.Context, channelID, content string) (*discordgo.Message, error) {
	spoken := speech.SanitizeForSpeech(content)
	if spoken == "" {
		return nil, errors.New("deliverVoiceReply: nothing to speak after sanitizing")
	}
	ogg, err := speech.SynthesizeOgg(ctx, spoken)
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
	if root := core.DiscordThreadRootFromSessionKey(sessionKey); root != "" {
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
	// COALESCE(t.plan_id, t.turn_id) — same fix as Deliver's own query above;
	// every plan-approval request today is keyed on the planning turn itself
	// (always top-level, so this was harmless in practice so far), but a
	// permission/decision UserInputRequest keyed on a plan-owned turn would
	// have hit the same zero-row join.
	err := a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, r.prompt, r.options
		FROM user_input_requests r
		JOIN turns t ON t.turn_id = r.turn_id
		JOIN turns root ON root.turn_id = COALESCE(t.plan_id, t.turn_id)
		JOIN sessions s ON s.session_key = root.parent_id
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

	// Superseded 2026-09-06 (delivery-in-the-loop): the approval prompt is a
	// short, fixed instruction again (plan_workflow.go's runApprovalGate) —
	// the plan itself is now delivered by a dedicated presentation turn via
	// deliver_reply/deliver_attachment before the gate ever opens, so this
	// prompt is never long enough to need its own overflow handling.
	send := &discordgo.MessageSend{Content: discordSendableContent(prompt)}
	if len(options) > 0 {
		send.Components = discordui.BuildUserInputComponents(requestID, options)
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

// deliverToolResult writes a model-tool-call's outcome back to `tool_calls`,
// the exact same contract activities/activities/tool_call.py's ToolCall
// activity honors (status/result/completed_at) — so the model's NEXT
// ModelCall sees a normal observation regardless of which language/process
// actually ran the call. Mirrors that file's `_finish_error`/success UPDATEs
// literally; kept here rather than shared since this is the only Go
// activity that writes into this Python-owned table.
func (a *discordDeliverActivity) deliverToolResult(ctx context.Context, toolCallID, status, result string) (types.ToolCallOutput, error) {
	if _, err := a.pool.Exec(ctx,
		"UPDATE tool_calls SET status = $2, result = $3, completed_at = now() WHERE tool_call_id = $1",
		toolCallID, status, result,
	); err != nil {
		return types.ToolCallOutput{}, err
	}
	return types.ToolCallOutput{ToolCallID: toolCallID, Status: status}, nil
}

// deliverToolChannelAndPrompt resolves a deliver_reply/deliver_attachment
// call's target channel + turn's session_key, the same join Deliver/
// DeliverInterim already use (and the same COALESCE(t.plan_id, t.turn_id)
// fix — deliver_reply/deliver_attachment are dispatched from plan-owned
// turns at least as often as top-level ones, so this one would have hit the
// zero-row bug immediately, not just in theory).
func (a *discordDeliverActivity) deliverToolChannelAndPrompt(ctx context.Context, turnID string) (channelID, sessionKey string, err error) {
	err = a.pool.QueryRow(ctx,
		`SELECT s.channel_id, s.session_key
		 FROM turns t
		 JOIN turns root ON root.turn_id = COALESCE(t.plan_id, t.turn_id)
		 JOIN sessions s ON s.session_key = root.parent_id
		 WHERE t.turn_id = $1`,
		turnID,
	).Scan(&channelID, &sessionKey)
	return
}

// DeliverReply is the deliver_reply model tool (docs/components/
// activities-outbound-delivery.md's model-driven retry philosophy, applied
// to delivery itself — llm.py's _DELIVER_REPLY_SCHEMA). Dispatched by
// turn.go straight to this connection's own embedded worker (same routing
// as Deliver/DeliverChunk/DeliverInterim), never through the generic
// tenant-worker ToolCall path — reads its own arguments from `tool_calls`
// by tool_call_id, same reference-passing contract every other tool call
// honors (tool_call.py's ToolCall is the reference implementation this
// mirrors). A defensive backstop only: the model is expected to keep each
// call under the limit itself (that's the whole point of offering the
// tool), this just guarantees the failure is legible if it doesn't.
func (a *discordDeliverActivity) DeliverReply(ctx context.Context, input types.ToolCallInput) (types.ToolCallOutput, error) {
	var argumentsJSON []byte
	var turnID string
	if err := a.pool.QueryRow(ctx,
		"SELECT arguments, parent_id FROM tool_calls WHERE tool_call_id = $1", input.ToolCallID,
	).Scan(&argumentsJSON, &turnID); err != nil {
		return types.ToolCallOutput{}, err
	}
	var args struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(argumentsJSON, &args); err != nil {
		return a.deliverToolResult(ctx, input.ToolCallID, "error", fmt.Sprintf(`{"error":"invalid arguments: %s"}`, err))
	}

	channelID, sessionKey, err := a.deliverToolChannelAndPrompt(ctx, turnID)
	if err != nil {
		return types.ToolCallOutput{}, err
	}

	if n := len([]rune(args.Content)); n > discordMessageLengthLimit {
		result := fmt.Sprintf(`{"error":"content_too_long","limit":%d,"actual":%d}`, discordMessageLengthLimit, n)
		return a.deliverToolResult(ctx, input.ToolCallID, "error", result)
	}

	msg, err := a.session.ChannelMessageSend(channelID, discordSendableContent(args.Content))
	if err != nil {
		return a.deliverToolResult(ctx, input.ToolCallID, "error", fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, args.Content)
	return a.deliverToolResult(ctx, input.ToolCallID, "ok", `{"delivered":true}`)
}

// DeliverAttachment is the deliver_attachment model tool — same contract as
// DeliverReply above, sends `content` as a file attachment instead of an
// inline message. In-memory only (strings.NewReader): nothing downstream
// needs the file to persist past this one send.
func (a *discordDeliverActivity) DeliverAttachment(ctx context.Context, input types.ToolCallInput) (types.ToolCallOutput, error) {
	var argumentsJSON []byte
	var turnID string
	if err := a.pool.QueryRow(ctx,
		"SELECT arguments, parent_id FROM tool_calls WHERE tool_call_id = $1", input.ToolCallID,
	).Scan(&argumentsJSON, &turnID); err != nil {
		return types.ToolCallOutput{}, err
	}
	var args struct {
		Content  string `json:"content"`
		Filename string `json:"filename"`
	}
	if err := json.Unmarshal(argumentsJSON, &args); err != nil {
		return a.deliverToolResult(ctx, input.ToolCallID, "error", fmt.Sprintf(`{"error":"invalid arguments: %s"}`, err))
	}
	filename := strings.TrimSpace(args.Filename)
	if filename == "" {
		filename = "attachment.txt"
	}

	channelID, sessionKey, err := a.deliverToolChannelAndPrompt(ctx, turnID)
	if err != nil {
		return types.ToolCallOutput{}, err
	}

	msg, err := a.session.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{
		Content: fmt.Sprintf("Attached: %s", filename),
		Files: []*discordgo.File{{
			Name:   filename,
			Reader: strings.NewReader(args.Content),
		}},
	})
	if err != nil {
		return a.deliverToolResult(ctx, input.ToolCallID, "error", fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, fmt.Sprintf("[attached %s]", filename))
	return a.deliverToolResult(ctx, input.ToolCallID, "ok", `{"delivered":true}`)
}

// DiscordDeliverChunk delivers one streamed sentence-chunk (docs/components/
// gateway.md's "Resolved: ModelCall Streaming") — creates the turn's
// message on the first chunk, edits it in place on every later one.
// Registered on the same embedded per-connection worker as Deliver above
// (discord.go's runConnection), since it needs the same live
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
	var streamedMessageOffset int
	// Same COALESCE(t.plan_id, t.turn_id) fix as Deliver's own query above —
	// streamingEligible (turn.go) doesn't check ParentType, so a plan-owned
	// turn's streaming-eligible first iteration hit this same zero-row join.
	err = a.pool.QueryRow(ctx, `
		SELECT s.channel_id, s.session_key, t.streamed_message_ref, t.streamed_message_offset
		FROM turns t
		JOIN turns root ON root.turn_id = COALESCE(t.plan_id, t.turn_id)
		JOIN sessions s ON s.session_key = root.parent_id
		WHERE t.turn_id = $1
	`, turnID).Scan(&channelID, &sessionKey, &streamedMessageRef, &streamedMessageOffset)
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

	// Real, live bug found 2026-09-06 (028_turns_streamed_message_offset.sql's
	// own comment has the full story): `content` is the CUMULATIVE text for
	// the whole turn so far, but content[streamed_message_offset:] — not
	// content itself — is what the CURRENTLY-EDITED message actually shows;
	// everything before that offset already belongs to an earlier, now-
	// finalized message from a prior rollover. Once the unflushed remainder
	// would exceed Discord's per-message length cap, roll over to a new
	// message instead of continuing to grow (and eventually 400-reject) the
	// old one.
	contentRunes := []rune(content)
	if streamedMessageOffset > len(contentRunes) {
		streamedMessageOffset = len(contentRunes) // defensive; should never happen
	}
	visible := contentRunes[streamedMessageOffset:]

	for len(visible) > discordMessageLengthLimit {
		head, rest := discordSplitForLimit(visible, discordMessageLengthLimit)
		headContent := discordSendableContent(string(head))
		if streamedMessageRef == nil {
			msg, err := a.session.ChannelMessageSend(channelID, headContent)
			if err != nil {
				return err
			}
			a.recordAmbientBotMessage(ctx, channelID, msg.ID, sessionKey, headContent)
			log.Printf("discord: turn %s streamed chunk %d filled message %s to the length cap, rolling over", turnID, seq, msg.ID)
		} else {
			if _, err := a.session.ChannelMessageEdit(channelID, *streamedMessageRef, headContent); err != nil {
				return err
			}
			a.recordAmbientBotMessage(ctx, channelID, *streamedMessageRef, sessionKey, headContent)
			log.Printf("discord: turn %s streamed chunk %d filled message %s to the length cap, rolling over", turnID, seq, *streamedMessageRef)
		}
		streamedMessageOffset += len(head)
		visible = rest
		streamedMessageRef = nil // every rolled-over message is finalized; the next segment always starts a fresh one
	}

	sendContent := discordSendableContent(string(visible))

	var activeMessageID string
	if streamedMessageRef == nil {
		msg, err := a.session.ChannelMessageSend(channelID, sendContent)
		if err != nil {
			return err
		}
		activeMessageID = msg.ID
		log.Printf("discord: turn %s streamed chunk %d created message %s", turnID, seq, msg.ID)
	} else {
		if _, err := a.session.ChannelMessageEdit(channelID, *streamedMessageRef, sendContent); err != nil {
			return err
		}
		activeMessageID = *streamedMessageRef
		log.Printf("discord: turn %s streamed chunk %d edited message %s", turnID, seq, *streamedMessageRef)
	}
	a.recordAmbientBotMessage(ctx, channelID, activeMessageID, sessionKey, sendContent)

	if _, err := a.pool.Exec(ctx,
		"UPDATE turns SET streamed_message_ref = $1, streamed_message_offset = $2 WHERE turn_id = $3",
		activeMessageID, streamedMessageOffset, turnID,
	); err != nil {
		return err
	}
	return markSent()
}
