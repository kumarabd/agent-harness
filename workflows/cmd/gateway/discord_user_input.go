package main

import (
	"context"
	"fmt"
	"log"
	"strings"

	"github.com/bwmarrin/discordgo"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// discordUserInputInteractionCreate handles a button click on a mid-turn
// user-input prompt pushed by DeliverInterim (deliver_discord.go). Same
// end-state as Web's handleRespond (respond.go): resolves the pending
// request's target workflow, calls SignalWorkflow with a
// types.UserInputResponse carrying the clicked option — the workflow's
// own signal handler wakes up and resumes its wait.
//
// docs/components/user-input.md, "Resolved: Response-Routing for Discord
// Text" (2026-08-28). This closes the second half of the mid-turn
// interim delivery gap — the push half landed 2026-08-27, but a user
// answering the pushed prompt still had no way to be recognized as
// answering *this specific request* rather than starting a new turn.
// Button custom_ids carry the request_id explicitly, so there's no
// ambiguity: a click IS an answer, an ordinary reply IS a new turn.
//
// Discord requires acknowledging every interaction within 3 seconds or
// it times out and shows "This interaction failed" to the user
// (verified directly against the API docs, not assumed). Every exit
// path from this function respondS to the interaction, even the error
// paths — the alternative is a silently-broken UX where the buttons
// stay clickable forever with no user-visible feedback that anything
// happened.
func (s *server) discordUserInputInteractionCreate(
	ctx context.Context,
	dg *discordgo.Session,
	ic *discordgo.InteractionCreate,
	connectionID string,
) {
	if ic.Type != discordgo.InteractionMessageComponent {
		return
	}
	data := ic.MessageComponentData()
	// Filter on the prefix rather than assuming this handler is only
	// called for user-input buttons — a future third button type on this
	// same gateway would otherwise silently be routed here too. Match
	// the exact prefix DeliverInterim's buildUserInputComponents writes.
	if !strings.HasPrefix(data.CustomID, "user_input:") {
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(data.CustomID, "user_input:"), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		s.respondInteractionError(dg, ic.Interaction, "Malformed button — this shouldn't happen.")
		return
	}
	requestID, optionID := parts[0], parts[1]

	// Defense against a forged custom_id: verify the request actually
	// belongs to a session whose sessions.channel_id matches the channel
	// the click landed in. Same join respond.go uses for Web (a caller
	// can never answer a request surfaced under a different user's
	// session_key) — for Discord, the equivalent is "you're in the
	// channel this request was raised in." Anyone in that channel can
	// answer, which matches how the push half already treats the
	// prompt: it's a channel-scoped message, not a per-user DM.
	var workflowID, status, channelID string
	err := s.pool.QueryRow(ctx, `
		SELECT r.workflow_id, r.status, s.channel_id
		FROM user_input_requests r
		JOIN turns t ON t.turn_id = r.turn_id
		JOIN sessions s ON s.session_key = t.parent_id
		WHERE r.request_id = $1
	`, requestID).Scan(&workflowID, &status, &channelID)
	if err != nil {
		s.respondInteractionError(dg, ic.Interaction, "That request no longer exists.")
		return
	}
	if channelID != ic.ChannelID {
		// The request exists but belongs to a different channel — either
		// a forged custom_id, or the button was somehow re-clicked from
		// a copy of the message in another channel. Reject without
		// leaking which one.
		s.respondInteractionError(dg, ic.Interaction, "This button can't be used from this channel.")
		return
	}
	if status != "pending" {
		// Already answered / expired / cancelled elsewhere (e.g. the
		// 1-hour timeout, or another human already clicked in a channel
		// with multiple viewers). Idempotent ack, not an error — same
		// shape /respond's own "already_..." branch uses.
		s.respondInteractionUpdate(dg, ic, fmt.Sprintf("(this request was already %s)", status))
		return
	}

	// Fire the signal BEFORE responding — if signalling fails, the user
	// sees an error, buttons stay live for a retry. If we responded
	// first (say to save the 3s deadline), a signal failure would leave
	// buttons dead but the workflow still waiting, which is worse.
	payload := types.UserInputResponse{
		RequestID:        requestID,
		SelectedOptionID: &optionID,
	}
	if err := s.temporal.SignalWorkflow(ctx, workflowID, "", wf.UserInputResponseSignalName, payload); err != nil {
		log.Printf("discord: user_input signal for %s failed: %v", requestID, err)
		s.respondInteractionError(dg, ic.Interaction, "Couldn't record that response — try again.")
		return
	}

	// Success: strip the buttons and update the message text inline with
	// which option the user picked. This is the confirmed UX choice
	// (2026-08-28) — one message, clean, no extra clutter. Discord's
	// InteractionResponseUpdateMessage type takes over the original
	// message the button was on.
	label := optionLabelFromComponents(ic.Message, data.CustomID)
	newContent := strings.TrimSpace(ic.Message.Content)
	if label != "" {
		newContent += "\n\n— You chose **" + label + "**."
	} else {
		// Fallback: no matching label found in the message components
		// (shouldn't happen for a well-formed button; keep going).
		newContent += "\n\n— Response recorded."
	}
	empty := []discordgo.MessageComponent{}
	if err := dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    newContent,
			Components: empty, // strips every ActionRow
		},
	}); err != nil {
		// The signal already fired — the response was recorded, only the
		// UI update failed. Log but don't error out.
		log.Printf("discord: failed to update user_input message for %s: %v", requestID, err)
	}
}

// optionLabelFromComponents looks up the label of the button matching
// customID inside a discordgo message's component tree. Used purely for
// the post-click confirmation text — a miss just returns "" and the
// caller uses a generic fallback message.
func optionLabelFromComponents(msg *discordgo.Message, customID string) string {
	if msg == nil {
		return ""
	}
	for _, row := range msg.Components {
		ar, ok := row.(*discordgo.ActionsRow)
		if !ok {
			continue
		}
		for _, c := range ar.Components {
			btn, ok := c.(*discordgo.Button)
			if !ok {
				continue
			}
			if btn.CustomID == customID {
				return btn.Label
			}
		}
	}
	return ""
}

// respondInteractionError sends a short ephemeral (only-visible-to-clicker)
// error reply. Ephemeral because the whole channel doesn't need to see
// an error message meant for the person who clicked.
func (s *server) respondInteractionError(dg *discordgo.Session, i *discordgo.Interaction, msg string) {
	if err := dg.InteractionRespond(i, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{
			Content: msg,
			Flags:   discordgo.MessageFlagsEphemeral,
		},
	}); err != nil {
		log.Printf("discord: failed to respond with error %q: %v", msg, err)
	}
}

// respondInteractionUpdate strips buttons and updates the message text —
// used for the "already answered" idempotent path where there's no
// signal to fire but the buttons should still go away rather than
// staying live and misleading.
func (s *server) respondInteractionUpdate(dg *discordgo.Session, ic *discordgo.InteractionCreate, addendum string) {
	newContent := strings.TrimSpace(ic.Message.Content) + "\n\n" + addendum
	empty := []discordgo.MessageComponent{}
	if err := dg.InteractionRespond(ic.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{
			Content:    newContent,
			Components: empty,
		},
	}); err != nil {
		log.Printf("discord: failed to update already-answered user_input message: %v", err)
	}
}
