package mobile

import "testing"

func TestDeltaFor(t *testing.T) {
	tests := []struct {
		name        string
		lastCum     string
		cum         string
		wantText    string
		wantReplace bool
	}{
		{"first chunk", "", "Hello", "Hello", true},
		{"append", "Hello", "Hello world", " world", false},
		{"no change", "Hello", "Hello", "", false},
		{"provider backtrack", "Hello world", "Hello there", "Hello there", true},
		{"shorter than last", "Hello world", "Hi", "Hi", true},
		{"unicode append", "café", "café au lait", " au lait", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text, replace := deltaFor(tt.lastCum, tt.cum)
			if text != tt.wantText || replace != tt.wantReplace {
				t.Fatalf("deltaFor(%q,%q) = (%q,%v), want (%q,%v)",
					tt.lastCum, tt.cum, text, replace, tt.wantText, tt.wantReplace)
			}
		})
	}
}

// Reassembling every emitted delta must reproduce the final cumulative content
// — the property the client relies on.
func TestDeltaForReassembles(t *testing.T) {
	snapshots := []string{"", "Th", "Think", "Thinking", "Thinking about", "Thinking about it"}
	var buf, last string
	for _, s := range snapshots {
		if s == "" {
			continue
		}
		text, replace := deltaFor(last, s)
		if replace {
			buf = text
		} else {
			buf += text
		}
		last = s
	}
	if want := snapshots[len(snapshots)-1]; buf != want {
		t.Fatalf("reassembled %q, want %q", buf, want)
	}
}

func TestTrailingCursor(t *testing.T) {
	i := func(n int) *int { return &n }
	tests := []struct {
		name   string
		maxSeq *int
		window int
		want   int
	}{
		{"no turns", nil, 20, 0},
		{"fewer than window", i(5), 20, 0},
		{"exactly window", i(20), 20, 0},
		{"more than window", i(50), 20, 30},
		{"first turn only", i(1), 20, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := trailingCursor(tt.maxSeq, tt.window); got != tt.want {
				t.Fatalf("trailingCursor(%v,%d) = %d, want %d", tt.maxSeq, tt.window, got, tt.want)
			}
		})
	}
}

func TestIsTerminal(t *testing.T) {
	for _, s := range []string{"completed", "failed", "cancelled"} {
		if !isTerminal(s) {
			t.Errorf("isTerminal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"running", "pending", "", "blocked"} {
		if isTerminal(s) {
			t.Errorf("isTerminal(%q) = true, want false", s)
		}
	}
}
