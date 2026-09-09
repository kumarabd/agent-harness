// Package types holds the JSON-serializable shapes shared across the Go workflow
// layer and the Python activity layer. Mirrored by hand in activities/activities/types.py —
// not code-generated in this slice. Field names use the JSON tag to match the Python
// side's snake_case exactly, since Temporal's default data converter is plain JSON.
//
// Reshaped 2026-08-14 for the reference-passing contract
// (docs/components/temporal-workflow.md, "Resolved: Reference-Passing Contract"
// and "Resolved: Reference/ID Schema"): every activity input/output that used
// to carry message content, tool arguments, or tool results now carries only
// IDs and control-flow metadata. Content-bearing fields (ModelResponse.Content,
// ToolCall.Arguments, ToolResult.Result/Reason/SideEffect as workflow-visible
// fields, TurnResult.FinalMessage) are gone — that data now lives exclusively
// in Postgres, read/written by the activity implementations directly.
package types

import "time"

// Usage mirrors a model call's token accounting, used by the turn workflow's
// inline token/cost budget check (components/temporal-workflow.md, Resolved:
// Stop-Condition Default Values). Numbers, not content — stays workflow-visible.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Message is used only at the one remaining content-crossing boundary: the
// coordinator's signal payload, and the InsertMessage activity's input
// (components/temporal-workflow.md, "Resolved: Reference/ID Schema" — this is
// the literal handoff point from an already-durable Temporal signal into
// Postgres, not a violation of the contract).
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content,omitempty"`
	// SpeakerID — docs/components/memory-slot.md "Resolved: Per-Message
	// Speaker Identity". The platform-native sender id of whoever actually
	// authored this message (gateway/core.MessageEvent.User), threaded
	// through untouched from Ingest(). Empty for any message this process
	// synthesizes itself (proactive wake fold-in, subagent kickoff content,
	// plan seed/revision text) — those have no real human sender.
	SpeakerID string `json:"speaker_id,omitempty"`
	// ClientMsgID — docs/components/gateway/mobile.md. The client-generated id
	// a mobile device stamps on a message it sends, threaded through so its
	// optimistic echo can dedupe when the message fans back out over the
	// stream. Empty for anything this process synthesizes.
	ClientMsgID string `json:"client_msg_id,omitempty"`
}

// TurnInput starts a Turn Workflow — top-level or, recursively, a subagent.
// No content: the coordinator/parent turn hands over only IDs. The inbound
// message itself is written to Postgres by the turn workflow's own
// InsertMessage activity call at the very start of its loop, sourced from the
// signal payload the coordinator is holding (session coordinator only, not
// the turn workflow) — see coordinator.go.
type TurnInput struct {
	SessionKey string `json:"session_key"`
	TurnID     string `json:"turn_id"`
	ParentType string `json:"parent_type"` // "session" | "turn" — see components/state-layer.md turns.parent_type
	// ParentID/TurnSeq are only meaningful for a top-level turn (ParentType ==
	// "session") — passed through so the turn workflow's start-of-turn
	// InsertMessage call can create its own turns row (components/state-layer.md:
	// "Turn workflow, via its own activities — inserts the row when the turn
	// starts"). For a subagent (ParentType == "turn"), ParentID is the parent
	// turn_id and TurnSeq is unused (nil) — matches the schema's partial unique
	// index, which only applies when parent_type = 'session'.
	ParentID       string  `json:"parent_id"`
	TurnSeq        *int    `json:"turn_seq,omitempty"`
	InitialMessage Message `json:"initial_message"`
	// ConnectionID — docs/components/gateway.md's "Resolved: Outbound Flow"
	// (2026-08-25 correction). Set only for sessions on a connection-based
	// platform (Discord today); empty for Web. Threaded straight through
	// from CoordinatorInput (coordinator.go copies it into every TurnInput
	// it builds) rather than looked up again here — the Gateway replica that
	// received the triggering message already knows its own resolved
	// connection_id (Discord: the bot's own user id via GET /users/@me), so
	// there's nothing left to derive. Used at delivery time to compute the
	// target task queue deterministically (deliver:{platform}:{connection_id}).
	ConnectionID string `json:"connection_id,omitempty"`
	// InitiatedBy — docs/components/proactivity.md. Provenance of the turn:
	// "user" (a real inbound message — the default), "intn:<id>" (an
	// IntentionWorkflow fired and the coordinator woke), or "plan" (a
	// planning / checkpoint turn under a PlanWorkflow). Threaded straight
	// through to the InsertMessage call that creates the turns row. Empty is
	// treated as "user".
	InitiatedBy string `json:"initiated_by,omitempty"`
	// PreInserted: the turns row + seq-0 message already exist (the coordinator's
	// startTurn helper did InsertMessage before starting this workflow). Skip
	// the start-of-turn InsertMessage.
	PreInserted bool `json:"pre_inserted,omitempty"`
	// OfferDeliveryTools: this turn's ModelCall calls offer deliver_reply/
	// deliver_attachment (docs/components/activities-outbound-delivery.md's
	// model-driven retry philosophy applied to delivery itself). Threaded into
	// every ModelCallInput this turn builds, mirrored by Python's
	// ModelCallInput.offer_delivery_tools. No caller sets it today (the
	// plan-presentation turn that used to is gone); turn.go's own bounded
	// post-Deliver-failure recovery round sets it directly on a raw
	// ModelCallInput instead.
	OfferDeliveryTools bool `json:"offer_delivery_tools,omitempty"`
}

// TurnResult is a Turn Workflow's return value. Deliberately holds no content
// — not even the final response text (components/temporal-workflow.md,
// "Resolved: Reference/ID Schema": "even the *result* handed back to a
// parent/coordinator shouldn't carry content"). A parent turn wanting a
// subagent's actual output reads it from Postgres via TurnID, same as
// everything else.
type TurnResult struct {
	TurnID     string `json:"turn_id"`
	StopReason string `json:"stop_reason"` // "no_tool_calls" | "max_iterations" | "max_retries" | "budget_exhausted"
	Iterations int    `json:"iterations"`
	// InterruptedDuringDelivery — docs/components/gateway/discord-voice.md's
	// "Resolved: Overlapping Speech / Interrupts" gap, closed 2026-08-25: a
	// signal arriving while this turn's connection-based delivery
	// (DiscordDeliver/VoiceDeliver) was still in flight cancels that
	// delivery rather than losing the signal. Since the turn's own
	// ModelCall loop has already finished by the time delivery runs, there's
	// no way to fold the new message back into THIS turn — instead it's
	// handed back here, and coordinator.go treats it exactly like a
	// freshly-arrived signal once this turn's future resolves, starting a
	// new turn with it rather than discarding it.
	InterruptedDuringDelivery *SignalPayload `json:"interrupted_during_delivery,omitempty"`
}

// SignalPayload is what SignalWithStart / a follow-up signal carries into the
// Session Coordinator (02-architecture-temporal-execution.md §3).
// ScriptedModelResponses is gone — test fixtures are written directly to
// _test_scripted_responses by the starter CLI, never passed through the
// workflow (see workflows/cmd/starter).
type SignalPayload struct {
	Message Message `json:"message"`
}

// WakePayload is what a fired IntentionWorkflow's FireIntention activity sends
// to the session CoordinatorWorkflow via the Wake signal
// (docs/components/proactivity.md, "The fire path"). No content beyond a short
// objective + reason; the coordinator synthesises the turn's seed message from
// it. IntentionID is the firing IntentionWorkflow's id, recorded as the turn's
// initiated_by ("intn:<IntentionID>").
type WakePayload struct {
	IntentionID string `json:"intention_id"`
	Objective   string `json:"objective"`
	Why         string `json:"why,omitempty"`
}

// ModelCallInput is ModelCall's only input — no content. ModelCall reads
// prior turn history from Postgres itself (it *is* the context-hydration
// step now) and looks up ContextSeq's scripted/real response.
type ModelCallInput struct {
	TurnID     string `json:"turn_id"`
	ContextSeq int    `json:"context_seq"`
	// docs/components/turn-pipeline.md, Phase 7 — the previous step's
	// report_status tier hint for this step, threaded through opaquely (the
	// workflow never interprets it, just copies ModelCallOutput.NextStep's
	// modality/tier into the next call's input). Empty on the very first call
	// of a turn — model_registry.default_hint() supplies {language, medium}.
	HintModality string `json:"hint_modality"`
	HintTier     string `json:"hint_tier"`
	// OfferDeliveryTools — mirrors types.TurnInput's field of the same name;
	// see there. Also set directly (without a TurnInput) by turn.go's local
	// post-Deliver-failure recovery round.
	OfferDeliveryTools bool `json:"offer_delivery_tools,omitempty"`
}

// ToolCallRef is one tool call minted by ModelCall — name/ID/dispatch-kind
// only, no arguments. The workflow uses this to decide Activity-vs-child-workflow
// dispatch; it never sees the arguments themselves.
type ToolCallRef struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	IsSubagent bool   `json:"is_subagent"`
	// IsAskUser — docs/components/turn-pipeline.md. The model called `ask_user`;
	// turn.go dispatches a UserInputRequestWorkflow child (Kind "question") and
	// parks the loop on it, same as a subagent child. Minted by ModelCall the
	// same way IsSubagent is. Never true alongside IsSubagent.
	IsAskUser bool `json:"is_ask_user,omitempty"`
	// docs/components/user-input.md — computed by ModelCall at mint time,
	// since that's the one place in this call chain that has the real
	// arguments in memory (workflow code never does, under the
	// reference-passing contract). Never true at the same time as
	// IsSubagent in this first pass — deliberately not designed for that
	// combination yet.
	RequiresApproval bool `json:"requires_approval"`
	// The resolved {server, tool} identity behind this call — "shell" + the
	// reduced command-name token for shell_exec, the real {server,tool} pair
	// for call_tool, or (docs/components/tool-registry.md, "Resolved:
	// Three-Layer Tool Taxonomy & Per-Task Resolution") the ToolDiscover
	// Capability's own {server,tool} for a per-task resolved call — set
	// whenever ModelCall knows the identity, not only when RequiresApproval
	// is true. Crosses the reference-passing boundary the same way ToolName
	// already does (dispatch/routing metadata, not the call's actual
	// arguments — components/multi-tenancy.md's "tool name... is
	// workflow-visible by design, not an accepted leak").
	Server string `json:"server,omitempty"`
	Tool   string `json:"tool,omitempty"`
}

// ModelCallOutput is ModelCall's only output — refs and control metadata, never
// content or arguments.
type ModelCallOutput struct {
	// Status — docs/components/turn-pipeline.md's output schema. The model's
	// declared turn lifecycle: "working" (the loop continues), "done" (deliver
	// the final response + terminate), "blocked" (parked on a user-input
	// request — handled in a later phase; for now runs as "working"). The one
	// field turn.go branches on for termination. Synthesized by ModelCall from
	// tool-call presence ("done" ⟺ no tool calls this step) until the model
	// authors it directly in a later phase.
	Status    string        `json:"status"`
	ToolCalls []ToolCallRef `json:"tool_calls"`
	Usage     Usage         `json:"usage"`
	// HasContent — the model's message this step had non-whitespace text.
	// turn.go's no-progress guard: a "working" step with no content AND no
	// tool calls, twice running, is a stuck model — end the turn rather than
	// burn the whole iteration ceiling on empty responses (future-work.md §4).
	HasContent bool `json:"has_content"`
	// docs/components/context-slot.md — the assembled context's estimated
	// size, computed fresh in Python each call (lcm.py's estimate_tokens)
	// since this workflow can't accumulate it itself across separate
	// turn-workflow executions the way it does per-turn budget spend below.
	ContextTokens int `json:"context_tokens"`
	// docs/components/context-slot.md, "Responsibilities" — the model
	// actually used this call's real context window (model_registry.py),
	// letting the compression threshold below be a fraction of it instead
	// of a fixed constant.
	ContextWindow int `json:"context_window"`
	// NextStep — docs/components/turn-pipeline.md. The model's note-to-self plus
	// the model it wants for the next step. Advisory: turn.go copies
	// Modality/Tier into the next ModelCallInput and logs the rest. nil is a
	// valid value (no hint). Synthesized from the previous declare_next_step_hint
	// mechanism until the model authors it directly.
	NextStep *NextStep `json:"next_step,omitempty"`
}

// NextStep is ModelCallOutput.NextStep — the model's advisory hint about what its
// next reasoning step needs. EstRemainingSteps feeds a later phase's
// iteration-ceiling negotiation; unused for now.
type NextStep struct {
	Note              string `json:"note,omitempty"`
	Modality          string `json:"modality,omitempty"`
	Tier              string `json:"tier,omitempty"`
	EstRemainingSteps int    `json:"est_remaining_steps,omitempty"`
}

// ToolCallInput is ToolCall's only input — it reads its own arguments from
// Postgres via this ID (components/temporal-workflow.md).
type ToolCallInput struct {
	ToolCallID string `json:"tool_call_id"`
}

// ToolCallOutput is ToolCall's only output — status, not result/reason/side_effect.
// Those stay in Postgres; the workflow only needs ok/error/cancelled to
// decide retry-count bookkeeping.
type ToolCallOutput struct {
	ToolCallID string `json:"tool_call_id"`
	Status     string `json:"status"` // "ok" | "error" | "cancelled"
}

// InsertMessageInput is the input for the message-insert activity — the one
// place content still crosses an activity input boundary, since it's the
// literal handoff from the coordinator's signal payload (already durable via
// Temporal signal history) into Postgres. messages.seq is computed by the
// activity itself (MAX(seq)+1 within its own turn), not passed in — it's a
// pure ordering/persistence concern, decoupled from ModelCallInput.ContextSeq
// (which is a separate, workflow-tracked fixture-lookup index — the two only
// coincidentally start at the same value, they track different things).
//
// IsTurnStart marks the one call per turn that also creates the turns row
// (components/state-layer.md: "Turn workflow, via its own activities —
// inserts the row when the turn starts"). On that call, if ParentType ==
// "turn" (a subagent), Message is ignored — the activity instead derives the
// subagent's own inbound content from its *own* tool_calls.arguments row
// (tool_call_id == this TurnID, already written by the parent's ModelCall
// call), since the workflow itself never has that content to pass along.
type InsertMessageInput struct {
	TurnID      string  `json:"turn_id"`
	Message     Message `json:"message"`
	IsTurnStart bool    `json:"is_turn_start"`
	ParentID    string  `json:"parent_id"`
	ParentType  string  `json:"parent_type"`
	TurnSeq     *int    `json:"turn_seq,omitempty"`
	// InitiatedBy — set only on the is_turn_start call; written to
	// turns.initiated_by (docs/components/proactivity.md). Empty → 'user'.
	InitiatedBy string `json:"initiated_by,omitempty"`
}

// UserInputOption is one selectable choice in a UserInputRequest.
// docs/components/user-input.md.
type UserInputOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// UserInputRequest is UserInputRequestWorkflow's input — kind-agnostic; the
// only thing this workflow does is durably wait for a response, however long
// that takes (docs/components/user-input.md, "Resolved: Why an Activity
// Can't Do This, a Workflow Can"). Context is opaque here — carries whatever
// the specific consumer (permission gating, a future decision-request
// consumer) needs, never interpreted by this workflow itself.
type UserInputRequest struct {
	RequestID     string            `json:"request_id"`
	TurnID        string            `json:"turn_id"`
	Kind          string            `json:"kind"` // "permission" | "question" (ask_user) | "decision" | ...
	Prompt        string            `json:"prompt"`
	Options       []UserInputOption `json:"options"`
	AllowFreeText bool              `json:"allow_free_text"`
	Context       map[string]any    `json:"context"`
}

// UserInputResponse is what the human actually answered.
type UserInputResponse struct {
	RequestID        string  `json:"request_id"`
	SelectedOptionID *string `json:"selected_option_id"`
	FreeText         *string `json:"free_text"`
}

// ApprovalGatedCallSpec is set on UserInputRequestWorkflowInput only when
// this request IS permission gating (docs/components/user-input.md,
// "Resolved: Permission Gating as the First Consumer" — an approval request
// is one case of a user input request, not a separate workflow type; an
// earlier version of this design kept them as two nested workflow types
// specifically so a plain decision request wouldn't carry tool-call-dispatch
// behavior it didn't need — an optional field on one workflow does that just
// as well, without an extra child-workflow hop). Nil for a plain decision
// request. ToolName is the real top-level tool ("shell_exec" | "call_tool"),
// needed for toolTimingFor once/if approved.
type ApprovalGatedCallSpec struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
}

// UserInputRequestWorkflowInput/Output wrap UserInputRequest/Response with
// the workflow-dispatch-only ApprovalGatedCall concern — kept off
// UserInputRequest itself, since that struct is also what's persisted
// verbatim to Postgres by the RequestUserInput activity and shouldn't carry
// a field that activity has no use for.
type UserInputRequestWorkflowInput struct {
	Request           UserInputRequest       `json:"request"`
	ApprovalGatedCall *ApprovalGatedCallSpec `json:"approval_gated_call,omitempty"`
	// SessionKey/ConnectionID — docs/components/user-input.md's "Mid-turn
	// interim delivery" (push half). Workflow-dispatch-only routing
	// metadata, same scoping rationale as ApprovalGatedCall above: kept off
	// UserInputRequest itself (which RequestUserInputActivity persists
	// verbatim to Postgres and has no use for these) rather than threaded
	// through as a second, driftable copy. UserInputRequestWorkflow needs
	// them BEFORE dispatching an interim-delivery activity, not just able to
	// look them up inside one — Temporal requires the task queue
	// (deliver:{platform}:{connection_id}) to be chosen at ExecuteActivity
	// call time, the same reason turn.go's own deliverConnectionBased
	// resolves platform/connection_id before dispatch rather than inside
	// the activity. ConnectionID empty (Web, or any platform with no
	// connection-lease concept) means no interim push at all — Web already
	// closes this gap via polling (gateway/web.md's handlePoll reading
	// pending_input directly), no push needed.
	SessionKey   string `json:"session_key,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
}

// ErrTypeContentTooLong — the Temporal ApplicationError type name Deliver
// (deliver_discord.go) uses when a final answer doesn't fit in one platform
// message, and turn.go's deliverConnectionBased checks for to decide whether
// to run its delivery-recovery round (deliver_reply/deliver_attachment)
// instead of just losing the response. Shared here (not a local const in
// either package) since both sides of the activity/workflow boundary need
// the exact same string.
const ErrTypeContentTooLong = "ContentTooLong"

type UserInputRequestWorkflowOutput struct {
	Response UserInputResponse `json:"response"`
	// Set only when ApprovalGatedCall was set on the input — the real
	// ToolCall activity's own result if approved, or a synthesized
	// "cancelled" result if denied/expired/interrupted. turn.go's
	// drainResult reads this directly; plain UserInputRequestWorkflow
	// consumers (a future decision-request use) never see it set.
	ToolCallOutput *ToolCallOutput `json:"tool_call_output,omitempty"`
}

// --- docs/components/proactivity.md — intentions ---

// IntentionInput is IntentionWorkflow's input, and its own ContinueAsNew
// carry-forward. The workflow id is IntentionID ("intn:<user>:<slug>"). A
// calendar-recurring intention is a Temporal Schedule that starts one of these
// per firing (Kind "time"), so the workflow itself only ever runs one of:
// "time"/"deadline" (one-shot), "condition"/"state"/"event" (poll loop),
// "inactivity" (idle timer restarted by a `reset` signal).
type IntentionInput struct {
	IntentionID string `json:"intention_id"`
	SessionKey  string `json:"session_key"` // whose CoordinatorWorkflow the fire wakes
	Objective   string `json:"objective"`
	Why         string `json:"why,omitempty"`
	Kind        string `json:"kind"`

	FireAt     time.Time     `json:"fire_at,omitempty"`     // one-shot: absolute wall-clock
	Probe      *ProbeSpec    `json:"probe,omitempty"`       // poll kinds
	PollEvery  time.Duration `json:"poll_every,omitempty"`  // poll kinds
	ExpiresAt  time.Time     `json:"expires_at,omitempty"`  // poll kinds: give up unfired
	IdleFor    time.Duration `json:"idle_for,omitempty"`    // inactivity
	FiredCount int           `json:"fired_count,omitempty"` // carried across ContinueAsNew
}

// ProbeSpec is what a poll-kind intention checks each cycle: run `Tool` (a
// call_tool "server/tool", or a builtin name) with `Args`, then judge `Predicate`
// (natural language) against the result.
type ProbeSpec struct {
	Tool      string         `json:"tool"`
	Args      map[string]any `json:"args,omitempty"`
	Predicate string         `json:"predicate"`
}

// IntentionReviseSignal — the `revise` signal payload. Only set fields apply.
type IntentionReviseSignal struct {
	Objective string        `json:"objective,omitempty"`
	Why       string        `json:"why,omitempty"`
	FireAt    time.Time     `json:"fire_at,omitempty"`
	PollEvery time.Duration `json:"poll_every,omitempty"`
}

// IntentionStatus is the `status` query result.
type IntentionStatus struct {
	IntentionID string `json:"intention_id"`
	Objective   string `json:"objective"`
	Kind        string `json:"kind"`
	State       string `json:"state"` // "armed" | "firing" | "expired"
	FiredCount  int    `json:"fired_count"`
}

// FireIntentionInput — FireIntention SignalWithStarts the session coordinator
// with a WakePayload built from this.
type FireIntentionInput struct {
	IntentionID string `json:"intention_id"`
	SessionKey  string `json:"session_key"`
	Objective   string `json:"objective"`
	Why         string `json:"why,omitempty"`
}

// CheckConditionInput / CheckConditionResult — CheckCondition runs a poll-kind
// intention's probe and judges its predicate.
type CheckConditionInput struct {
	IntentionID string    `json:"intention_id"`
	Probe       ProbeSpec `json:"probe"`
}

type CheckConditionResult struct {
	Fired bool   `json:"fired"`
	Note  string `json:"note,omitempty"`
}
