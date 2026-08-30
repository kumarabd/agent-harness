package core

import "testing"

func TestSessionKeyFor(t *testing.T) {
	cases := []struct {
		platform, channelID, discriminator, want string
	}{
		// Web's default/main session keeps its exact pre-existing format.
		{"web", "user_abc", "channel:user_abc", "agent:main:web:user:user_abc"},
		// A branched Web session embeds the client-generated id.
		{"web", "user_abc", "session:brnch1", "agent:main:web:user:user_abc:session:brnch1"},
		{"discord", "123", "channel:123", "agent:main:discord:channel:123"},
		{"discord", "123", "reply_to_platform_message_id:root9", "agent:main:discord:channel:123:thread:root9"},
		{"discord-voice", "456", "channel:456", "agent:main:discord-voice:channel:456"},
	}
	for _, c := range cases {
		if got := SessionKeyFor(c.platform, c.channelID, c.discriminator); got != c.want {
			t.Errorf("SessionKeyFor(%q,%q,%q) = %q, want %q", c.platform, c.channelID, c.discriminator, got, c.want)
		}
	}
}

func TestDiscordThreadRootFromSessionKey(t *testing.T) {
	// Round-trips SessionKeyFor's own discord thread format.
	threadKey := SessionKeyFor("discord", "123", "reply_to_platform_message_id:root9")
	if got := DiscordThreadRootFromSessionKey(threadKey); got != "root9" {
		t.Errorf("thread key %q -> root %q, want root9", threadKey, got)
	}
	mainKey := SessionKeyFor("discord", "123", "channel:123")
	if got := DiscordThreadRootFromSessionKey(mainKey); got != "" {
		t.Errorf("main channel key %q -> root %q, want empty", mainKey, got)
	}
}
