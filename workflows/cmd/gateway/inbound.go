package main

import (
	"context"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// MessageEvent is the generic/agentic boundary docs/components/gateway.md's
// "Resolved: Inbound Flow" step 2 names but never wrote a concrete shape
// for. Platform-specific code (handleSend, for Web) normalizes its own raw
// payload into this; submitMessageEvent below implements the
// platform-agnostic remainder of that flow (steps 3-6) against it — nothing
// past this struct is Web-specific.
type MessageEvent struct {
	// Platform this event came from — "web" today, "discord"/"slack" once
	// built.
	Platform string
	// ChannelID scopes the SESSION — shared across every user posting in
	// that channel (a Discord channel, a Slack channel). Deliberately NOT
	// the same thing as User below: conflating the two would merge
	// different users' conversations into one session, or treat a shared
	// group channel as belonging to whichever user happened to post last.
	ChannelID string
	// User is who sent THIS message, within ChannelID — orthogonal to
	// session scoping. For Web, User and ChannelID are currently the same
	// Clerk user_id (no group-chat concept there), but kept as a separate
	// field so a group-chat platform doesn't need this struct to change
	// shape later. Not yet persisted anywhere past this struct — messages
	// has no author/platform_user_id column today, only role; a real gap
	// for group-chat attribution, but nothing exposes it yet since Web has
	// no group chats.
	User string
	// Content is the message text.
	Content string
	// PlatformMessageID is the idempotency/dedup key against
	// ingested_messages(platform, platform_message_id) — gateway.md's
	// "Resolved: Inbound Flow" step 4. For Web this is the client-generated
	// client_message_id (no platform-native message id exists for a
	// same-request HTTP POST); for a webhook platform it would be that
	// platform's own message/event id.
	PlatformMessageID string
}

// submitMessageEvent implements gateway.md's "Resolved: Inbound Flow" steps
// 3-6: resolve session_key, dedup check-then-insert against
// ingested_messages, SignalWithStart the session coordinator, ack. Returns
// "accepted" or "already_accepted" (step 4's dedup short-circuit) — the
// same response shape /send has always returned.
func (s *server) submitMessageEvent(ctx context.Context, event MessageEvent) (string, error) {
	sessionKey := sessionKeyFor(event.Platform, event.ChannelID)

	// Upsert with real values, before SignalWithStart — replaces the real
	// Gateway InsertMessageActivity's own 'unknown'/'unknown' placeholder
	// upsert used before a real Gateway existed (session-filesystem.md's
	// Notes Log). ON CONFLICT DO NOTHING on both writes below means
	// whichever write lands first wins — since this runs before the signal
	// that eventually triggers InsertMessageActivity, this one wins.
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO sessions (session_key, platform, channel_id) VALUES ($1, $2, $3) "+
			"ON CONFLICT (session_key) DO NOTHING",
		sessionKey, event.Platform, event.ChannelID,
	); err != nil {
		return "", err
	}

	// Real PRIMARY KEY, not an app-level check — a race between two
	// identical sends (e.g. a client retry) is resolved by the second
	// INSERT failing, not by application logic (same reasoning gateway.md's
	// own "Resolved: Connection Leasing" section relies on for dedup being
	// free regardless of concurrent writers).
	tag, err := s.pool.Exec(ctx,
		"INSERT INTO ingested_messages (platform, platform_message_id, session_key) "+
			"VALUES ($1, $2, $3) ON CONFLICT (platform, platform_message_id) DO NOTHING",
		event.Platform, event.PlatformMessageID, sessionKey,
	)
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		// Already durably submitted by an earlier attempt at this exact
		// platform_message_id — ack without re-signaling.
		return "already_accepted", nil
	}

	// SignalWithStart is the durable submission; the gateway never writes
	// the message body to Postgres itself (that happens later, inside the
	// coordinator/turn flow, sourced from the signal payload).
	opts := client.StartWorkflowOptions{
		ID:                    sessionKey,
		TaskQueue:             s.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}
	payload := types.SignalPayload{Message: types.Message{Role: "user", Content: event.Content}}
	if _, err := s.temporal.SignalWithStartWorkflow(
		ctx, sessionKey, wf.NewMessageSignalName, payload, opts,
		wf.CoordinatorWorkflow, wf.CoordinatorInput{SessionKey: sessionKey},
	); err != nil {
		return "", err
	}

	return "accepted", nil
}
