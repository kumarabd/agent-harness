package mobile

import (
	"strings"
	"testing"
)

func TestSummaryForBriefing(t *testing.T) {
	if got := summaryForBriefing("first\n\nsecond\tthird"); got != "first second third" {
		t.Fatalf("summary = %q", got)
	}

	long := strings.Repeat("🙂", 121)
	got := summaryForBriefing(long)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("missing ellipsis: %q", got)
	}
	if n := len([]rune(got)); n != 120 {
		t.Fatalf("rune count = %d, want 120", n)
	}
}
