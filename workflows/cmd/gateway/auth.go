package main

import (
	"context"
	"net/http"
	"strings"
)

type contextKey string

const clerkUserIDKey contextKey = "clerk_user_id"

// requireClerkAuth verifies the bearer token against the Clerk project's own
// public JWKS (clerk.go, docs/components/gateway/web.md "Resolved: Auth —
// Reuse agent-web's Existing Clerk Integration") and resolves the real Clerk
// user_id from the token's sub claim — session_key is built from this, never
// trusted from anything the client sends directly.
func requireClerkAuth(cfg clerkConfig, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if token == "" || token == authHeader {
			http.Error(w, "missing bearer token", http.StatusUnauthorized)
			return
		}

		sub, err := verifyClerkSessionJWT(r.Context(), cfg, token)
		if err != nil {
			http.Error(w, "invalid session token", http.StatusUnauthorized)
			return
		}

		ctx := context.WithValue(r.Context(), clerkUserIDKey, sub)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func userIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(clerkUserIDKey).(string)
	return id
}

// sessionKeyFor derives a session_key from a MessageEvent's Platform +
// ChannelID + Discriminator (inbound.go) — the session-scoping identity,
// deliberately NOT a function of User too (a group channel's session is
// shared across every user posting in it; see MessageEvent's own doc
// comment). Only place this string gets built, so the format only needs to
// be right in one location.
//
// discriminator is always populated by the caller (gateway.md's "Resolved:
// Multi-Session Channels" — never a magic empty string), "channel:{id}" for
// a channel's main session or "<type>:<id>" for a reply/thread-scoped one
// (e.g. Discord's "reply_to_platform_message_id:{rootID}", Web's own
// client-generated "session:{id}" — see websession.go). Whether that shows
// up in the actual session_key STRING is a separate, per-platform choice
// from whether the concept exists:
//   - "web": "channel:{channelID}" (the default/main session) produces the
//     EXACT unchanged "agent:main:web:user:{id}" format, byte-for-byte, to
//     avoid orphaning the already-running real production session. Any
//     other discriminator ("session:{id}", a client-generated branch —
//     websession.go's own webDiscriminator) embeds directly, same as
//     Discord — no backward-compatibility constraint on a NEW branch, only
//     on the pre-existing default.
//   - "discord": no backward-compatibility constraint at all (nothing real
//     was ever running before Discriminator existed), so it's always
//     embedded directly.
func sessionKeyFor(platform, channelID, discriminator string) string {
	switch platform {
	case "web":
		if discriminator == "channel:"+channelID {
			return "agent:main:web:user:" + channelID
		}
		_, id, ok := strings.Cut(discriminator, ":")
		if !ok {
			panic("sessionKeyFor: malformed discriminator " + discriminator)
		}
		return "agent:main:web:user:" + channelID + ":session:" + id
	case "discord":
		if discriminator == "channel:"+channelID {
			return "agent:main:discord:channel:" + channelID
		}
		// "<type>:<id>" — only the id half goes into the key; the type half
		// (e.g. "reply_to_platform_message_id") is resolution metadata, not
		// part of the session's own identity.
		_, id, ok := strings.Cut(discriminator, ":")
		if !ok {
			panic("sessionKeyFor: malformed discriminator " + discriminator)
		}
		return "agent:main:discord:channel:" + channelID + ":thread:" + id
	case "discord-voice":
		// docs/components/gateway/discord-voice.md — no reply-threading
		// concept for voice (Discriminator is always "channel:"+channelID,
		// discord_voice.go never sets anything else), so this is the whole
		// story: one session per joined voice channel, no thread variant.
		return "agent:main:discord-voice:channel:" + channelID
	default:
		panic("sessionKeyFor: unsupported platform " + platform)
	}
}

// discordThreadRootFromSessionKey is sessionKeyFor's own "discord" case run
// backward: a reply-scoped session's key already durably encodes the reply
// chain's root platform_message_id ("agent:main:discord:channel:{id}:thread:{rootID}"),
// so recovering it needs no new column — the session_key IS the record
// (gateway.md's "Resolved: Multi-Session Channels" — the graph lives in
// Postgres, and this is exactly the Postgres data it lives as).
//
// deliver_discord.go's own use: recording the bot's own sent message into
// discord_ambient_messages with the correct reply_to_platform_message_id, so
// a later human reply to that message continues resolving to the same
// thread root instead of resolveDiscordThreadRoot hitting a dead end on the
// bot's message and falling back to the main channel session — the real gap
// this closes (gateway/discord.md's "Discord-side reply-chain resolution
// past the bot's own messages").
//
// Returns "" for the main channel session (no ":thread:" suffix) — a plain,
// non-threaded answer isn't itself a reply to anything, matching how a
// human's own first message in a chain has no reply_to_platform_message_id
// either.
func discordThreadRootFromSessionKey(sessionKey string) string {
	_, root, ok := strings.Cut(sessionKey, ":thread:")
	if !ok {
		return ""
	}
	return root
}
