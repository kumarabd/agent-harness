package mobile

import "encoding/json"

// The WebSocket wire protocol — docs/components/gateway/mobile.md.
//
// Every frame is a JSON object with a "type" field. Server→client frames carry
// a turn_seq so the client can order them and persist its resume cursor
// (client-owned: the server keeps no per-device read state).

// --- server → client ---

type turnStartFrame struct {
	Type        string `json:"type"` // "turn_start"
	TurnSeq     int    `json:"turn_seq"`
	TurnID      string `json:"turn_id"`
	InitiatedBy string `json:"initiated_by,omitempty"` // "user" | "intn:<id>"
}

type messageFrame struct {
	Type      string `json:"type"` // "message"
	TurnSeq   int    `json:"turn_seq"`
	Seq       int    `json:"seq"` // ordering within the turn
	Role      string `json:"role"`
	Content   string `json:"content"`
	SpeakerID string `json:"speaker_id,omitempty"` // the human (Clerk sub) — same across every one of this user's devices
	// DeviceID — first-party-plan.md §2. Which device sent this; use this
	// (not SpeakerID) for "sent from your iPad" multi-device rendering.
	DeviceID    string `json:"device_id,omitempty"`
	ClientMsgID string `json:"client_msg_id,omitempty"` // echoes the sender's id for optimistic-echo dedupe
}

type deltaFrame struct {
	Type    string `json:"type"` // "delta"
	TurnSeq int    `json:"turn_seq"`
	Seq     int    `json:"seq"`
	Text    string `json:"text"`
	Replace bool   `json:"replace,omitempty"` // true ⇒ reset the streamed buffer to Text (resume snapshot / provider backtrack)
}

type statusFrame struct {
	Type    string `json:"type"` // "status" — a transient progress ping, not a chat message
	TurnSeq int    `json:"turn_seq"`
	Text    string `json:"text"`
}

type turnEndFrame struct {
	Type    string `json:"type"` // "turn_end"
	TurnSeq int    `json:"turn_seq"`
	Status  string `json:"status"` // "completed" | "failed" | "cancelled"
}

type askUserFrame struct {
	Type          string          `json:"type"` // "ask_user"
	TurnSeq       int             `json:"turn_seq"`
	RequestID     string          `json:"request_id"`
	Kind          string          `json:"kind"`
	Prompt        string          `json:"prompt"`
	Options       json.RawMessage `json:"options,omitempty"`
	AllowFreeText bool            `json:"allow_free_text"`
}

type errorFrame struct {
	Type    string `json:"type"` // "error"
	Message string `json:"message"`
}

type resumedFrame struct {
	Type           string `json:"type"` // "resumed" — ack after replay, before live
	ThroughTurnSeq int    `json:"through_turn_seq"`
}

// --- client → server ---

type inboundFrame struct {
	// Type — "auth" | "message" | "answer" | "cancel" | "resume". "cancel"
	// needs no fields beyond this (docs/components/gateway/first-party-plan.md's
	// cancel/stop primitive) — it stops whatever turn is currently running in
	// this connection's own session, ownership-checked server-side.
	Type string `json:"type"`
	// auth
	Token string `json:"token,omitempty"`
	// resume (sent with auth, or standalone)
	AfterTurnSeq *int `json:"after_turn_seq,omitempty"`
	// message
	ClientMsgID string `json:"client_msg_id,omitempty"`
	Text        string `json:"text,omitempty"`
	// answer (ask_user)
	RequestID        string `json:"request_id,omitempty"`
	SelectedOptionID string `json:"selected_option_id,omitempty"`
	FreeText         string `json:"free_text,omitempty"`
	// device id — stamped on the user message as speaker_id, and used for
	// presence + "sent from another device" attribution on other devices.
	DeviceID string `json:"device_id,omitempty"`
}
