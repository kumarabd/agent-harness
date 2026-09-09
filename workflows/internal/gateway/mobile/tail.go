package mobile

// Pure helpers for the outbound tail (conn.go). Kept separate so the cursor
// math and the cumulative→delta diffing are unit-testable without a socket or
// a database.

// deltaFor turns a cumulative streamed-content snapshot into what the client
// should apply. turn_deliveries.content is cumulative (Discord edits a message
// in place); the mobile client wants incremental appends. When the new snapshot
// extends the last one we send just the tail; otherwise (first chunk after a
// (re)connect, or a provider that backtracked) we send the whole snapshot with
// replace=true so the client resets its buffer.
func deltaFor(lastCum, cum string) (text string, replace bool) {
	if lastCum != "" && len(cum) >= len(lastCum) && cum[:len(lastCum)] == lastCum {
		return cum[len(lastCum):], false
	}
	return cum, true
}

// trailingCursor is the resume point for a client that sent no cursor: the
// start of the last `window` turns (never below 0). maxSeq is nil when the
// session has no turns yet.
func trailingCursor(maxSeq *int, window int) int {
	if maxSeq == nil {
		return 0
	}
	s := *maxSeq - window
	if s < 0 {
		s = 0
	}
	return s
}

// isTerminal reports whether a turn status means the turn is done and its
// cursor can advance past it.
func isTerminal(status string) bool {
	switch status {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}
