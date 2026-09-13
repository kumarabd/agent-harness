package web

import "testing"

func TestSessionDiscriminator(t *testing.T) {
	if got := sessionDiscriminator("user_1", ""); got != "channel:user_1" {
		t.Fatalf("empty session: got %q", got)
	}
	if got := sessionDiscriminator("user_1", "main"); got != "channel:user_1" {
		t.Fatalf("main session: got %q", got)
	}
	if got := sessionDiscriminator("user_1", "child_1"); got != "session:child_1" {
		t.Fatalf("child session: got %q", got)
	}
}

func TestSessionIDFromKeyUsesPlatformNamespace(t *testing.T) {
	tests := []struct {
		platform string
		key      string
		want     string
	}{
		{"web", "agent:main:web:user:user_1", "main"},
		{"web", "agent:main:web:user:user_1:session:child_1", "child_1"},
		{"macos", "agent:main:macos:user:user_1", "main"},
		{"macos", "agent:main:macos:user:user_1:session:child_1", "child_1"},
	}
	for _, tt := range tests {
		if got := sessionIDFromKey(tt.platform, "user_1", tt.key); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.platform, got, tt.want)
		}
	}
}
