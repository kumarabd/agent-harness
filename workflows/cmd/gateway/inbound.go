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
	// Discriminator — gateway.md's "Resolved: Multi-Session Channels" —
	// which of possibly-many sessions in ChannelID this event belongs to.
	// ALWAYS populated by the caller, never empty: "channel:{channelID}" for
	// a channel's own main session, or "<type>:<id>" (e.g. Discord's
	// "reply_to_platform_message_id:{rootID}") for a reply/thread-scoped
	// one. Fed into sessionKeyFor alongside Platform/ChannelID.
	Discriminator string
	// ParentSessionKey — gateway.md's "Resolved: Multi-Session Channels" —
	// set only when Discriminator resolves to a session that doesn't exist
	// yet (detected via the sessions INSERT's own RowsAffected below, not a
	// separate lookup): which session this new one branched from. Empty for
	// a channel's own main session, which has no parent.
	ParentSessionKey string
	// ConnectionID — gateway.md's "Resolved: Outbound Flow" (2026-08-25
	// correction). Set only on a connection-based platform (Discord: the
	// bot's own user id, resolved by the caller once at startup via GET
	// /users/@me — never re-derived here); empty for Web. Unlike
	// ParentSessionKey, passed to CoordinatorInput on EVERY call, not just
	// genesis — see CoordinatorInput's own doc comment for why.
	ConnectionID string
}

// submitMessageEvent implements gateway.md's "Resolved: Inbound Flow" steps
// 3-6: resolve session_key, dedup check-then-insert against
// ingested_messages, SignalWithStart the session coordinator, ack. Returns
// "accepted" or "already_accepted" (step 4's dedup short-circuit) — the
// same response shape /send has always returned.
func (s *server) submitMessageEvent(ctx context.Context, event MessageEvent) (string, error) {
	sessionKey := sessionKeyFor(event.Platform, event.ChannelID, event.Discriminator)

	// Upsert with real values, before SignalWithStart — replaces the real
	// Gateway InsertMessageActivity's own 'unknown'/'unknown' placeholder
	// upsert used before a real Gateway existed (session-filesystem.md's
	// Notes Log). ON CONFLICT DO NOTHING on both writes below means
	// whichever write lands first wins — since this runs before the signal
	// that eventually triggers InsertMessageActivity, this one wins.
	//
	// parent_session_key: NULL when ParentSessionKey is unset (a channel's
	// own main session has no parent); ON CONFLICT DO NOTHING means this
	// only ever takes effect on the FIRST insert for a given session_key —
	// exactly genesis, never overwritten by a later message for the same
	// session.
	var parentSessionKey *string
	if event.ParentSessionKey != "" {
		parentSessionKey = &event.ParentSessionKey
	}
	// gateway.md's "Resolved: Multi-Session Channels" — genesis detection is
	// free from state already being written: RowsAffected() > 0 means this
	// is genuinely the first message this session_key has ever seen. This
	// is the one moment CoordinatorWorkflow's own LCM-copy context injection
	// (coordinator.go) needs to fire — CoordinatorInput.ParentSessionKey
	// below is set ONLY on this exact condition, never on a later message
	// for an already-existing session, regardless of what event.ParentSessionKey
	// itself carries (harmless if the caller sends it on every message for
	// a branch — this check is what actually gates the effect).
	sessionsTag, err := s.pool.Exec(ctx,
		"INSERT INTO sessions (session_key, platform, channel_id, parent_session_key) VALUES ($1, $2, $3, $4) "+
			"ON CONFLICT (session_key) DO NOTHING",
		sessionKey, event.Platform, event.ChannelID, parentSessionKey,
	)
	if err != nil {
		return "", err
	}
	isGenesis := sessionsTag.RowsAffected() > 0

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
	//
	// coordinatorInput.ParentSessionKey only set on isGenesis — these
	// start-args are only actually consulted by Temporal if this call is
	// the one that truly starts the workflow (an already-running execution
	// just gets signaled, ignoring them), which for a brand-new session_key
	// always coincides with isGenesis anyway; gating on isGenesis explicitly
	// rather than relying on that coincidence is what keeps this correct
	// across this session's OWN later idle-timeout restarts too.
	coordinatorInput := wf.CoordinatorInput{SessionKey: sessionKey, ConnectionID: event.ConnectionID}
	if isGenesis {
		coordinatorInput.ParentSessionKey = event.ParentSessionKey
	}
	opts := client.StartWorkflowOptions{
		ID:                    sessionKey,
		TaskQueue:             s.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}
	payload := types.SignalPayload{Message: types.Message{Role: "user", Content: event.Content}}
	if _, err := s.temporal.SignalWithStartWorkflow(
		ctx, sessionKey, wf.NewMessageSignalName, payload, opts,
		wf.CoordinatorWorkflow, coordinatorInput,
	); err != nil {
		return "", err
	}

	return "accepted", nil
}
