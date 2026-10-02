package ids

import "testing"

func TestUserScopeOf(t *testing.T) {
	cases := map[string]string{
		"agent:main:web:user:abc":                   "agent:main:web:user:abc",
		"agent:main:web:user:abc:session:xyz":       "agent:main:web:user:abc",
		"agent:main:discord:channel:123":            "agent:main:discord:channel:123",
		"agent:main:discord:channel:123:thread:456": "agent:main:discord:channel:123",
		"agent:main:discord-voice:channel:9":        "agent:main:discord-voice:channel:9",
	}
	for in, want := range cases {
		if got := UserScopeOf(in); got != want {
			t.Errorf("UserScopeOf(%q) = %q, want %q", in, got, want)
		}
	}
}
