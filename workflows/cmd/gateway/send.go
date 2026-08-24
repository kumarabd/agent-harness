package main

import (
	"encoding/json"
	"net/http"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

type sendRequest struct {
	Content         string `json:"content"`
	ClientMessageID string `json:"client_message_id"`
}

type sendResponse struct {
	Status string `json:"status"`
}

// handleSend — docs/components/gateway.md, "Resolved: Inbound Flow", steps
// 2-6. Step 1 (normalize a raw platform event into a generic MessageEvent)
// is trivial for Web: the request body already IS the generic shape, no
// platform-specific parsing needed. No separate MessageEvent struct exists
// yet (that doc's own still-open "generic MessageEvent shape" item) — this
// handler is the one real instance to eventually check that shape against.
func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req sendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Content == "" || req.ClientMessageID == "" {
		http.Error(w, "content and client_message_id are required", http.StatusBadRequest)
		return
	}

	userID := userIDFromContext(r.Context())
	sessionKey := sessionKeyFor(userID)
	ctx := r.Context()

	// Upsert with real values, before SignalWithStart — this is the real
	// Gateway InsertMessageActivity's own 'unknown'/'unknown' placeholder
	// upsert was always meant to be replaced by
	// (components/session-filesystem.md's Notes Log: "a real Gateway
	// replaces this outright rather than needing to get placeholder values
	// right"). ON CONFLICT DO NOTHING on both sides means whichever write
	// lands first wins — since this runs before the signal that eventually
	// triggers InsertMessageActivity, this one wins.
	if _, err := s.pool.Exec(ctx,
		"INSERT INTO sessions (session_key, platform, channel_id) VALUES ($1, 'web', $2) "+
			"ON CONFLICT (session_key) DO NOTHING",
		sessionKey, userID,
	); err != nil {
		http.Error(w, "failed to record session", http.StatusInternalServerError)
		return
	}

	// docs/components/gateway.md, "Resolved: Inbound Flow" step 4 — dedup
	// check-then-insert against ingested_messages(platform,
	// platform_message_id). Real PRIMARY KEY, not an app-level check — a
	// race between two identical sends (e.g. a client retry) is resolved by
	// the second INSERT failing, not by application logic (same reasoning
	// gateway.md's own "Resolved: Connection Leasing" section relies on for
	// dedup being free regardless of concurrent writers).
	tag, err := s.pool.Exec(ctx,
		"INSERT INTO ingested_messages (platform, platform_message_id, session_key) "+
			"VALUES ('web', $1, $2) ON CONFLICT (platform, platform_message_id) DO NOTHING",
		req.ClientMessageID, sessionKey,
	)
	if err != nil {
		http.Error(w, "failed to record inbound message", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		// Already durably submitted by an earlier attempt at this exact
		// client_message_id — ack without re-signaling (gateway.md step 4).
		writeJSON(w, http.StatusOK, sendResponse{Status: "already_accepted"})
		return
	}

	// docs/components/gateway.md, "Resolved: Inbound Flow" step 5 —
	// SignalWithStart is the durable submission; the gateway never writes
	// the message body to Postgres itself (that happens later, inside the
	// coordinator/turn flow, sourced from the signal payload).
	opts := client.StartWorkflowOptions{
		ID:                    sessionKey,
		TaskQueue:             s.taskQueue,
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}
	payload := types.SignalPayload{Message: types.Message{Role: "user", Content: req.Content}}
	if _, err := s.temporal.SignalWithStartWorkflow(
		ctx, sessionKey, wf.NewMessageSignalName, payload, opts,
		wf.CoordinatorWorkflow, wf.CoordinatorInput{SessionKey: sessionKey},
	); err != nil {
		http.Error(w, "failed to submit message", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, sendResponse{Status: "accepted"})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
