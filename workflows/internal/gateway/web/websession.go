package web

import "strings"

// webMainSessionID is the stable, addressable sentinel representing Web's
// default/main session in the client-facing session_id vocabulary — never
// the empty string, so GET /sessions can list it like any other real
// session (gateway.md's "Resolved: Multi-Session Channels" already
// disciplines Discriminator itself to never rely on a magic empty-string
// case; this extends the same discipline to the client-facing session_id).
// Internally still maps to the exact unchanged "channel:{channelID}"
// discriminator / "agent:main:web:user:{id}" session_key format — nothing
// about the real, already-verified production session changes.
const webMainSessionID = "main"

// webDiscriminator maps an opaque, client-supplied Web session_id (empty or
// "main" both mean the default) to a MessageEvent.Discriminator. sessionID
// is never trusted to be a full session_key or ChannelID — channelID always
// comes from the authenticated Clerk user_id (userIDFromContext), never
// anything the client sends directly. Unlike Discord, there's no
// platform-native reply/thread concept to derive a discriminator from, so a
// branched Web session's id is simply whatever opaque value the UI
// generated itself when the user branched — the client remembers and
// resends it on every later message for that session, same "no server-side
// lookup needed to resolve it" property Discord's deterministic reply-chain
// root has, just sourced differently.
func webDiscriminator(channelID, sessionID string) string {
	if sessionID == "" || sessionID == webMainSessionID {
		return "channel:" + channelID
	}
	return "session:" + sessionID
}

// webSessionIDFromKey reverses webDiscriminator/core.SessionKeyFor's own Web
// format for GET /sessions — parses a real session_key belonging to this
// user back into the opaque session_id the client understands. Only ever
// called on rows already filtered to this exact user's own channel_id
// (handleListSessions's own query), so the prefix match is guaranteed.
func webSessionIDFromKey(channelID, sessionKey string) string {
	mainKey := "agent:main:web:user:" + channelID
	if sessionKey == mainKey {
		return webMainSessionID
	}
	return strings.TrimPrefix(sessionKey, mainKey+":session:")
}
