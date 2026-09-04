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
	// --- docs/components/request-pipeline/08-planning.md, Phase 3C ---
	// PreInserted: the turns row + seq-0 message already exist (the dispatch
	// helper did InsertMessage before deciding what workflow to start). Skip
	// the start-of-turn InsertMessage.
	PreInserted bool `json:"pre_inserted,omitempty"`
	// PlanningMode: this turn drafts a checkpoint plan (one ModelCall, planning
	// system prompt, `propose_plan` tool) rather than running the task. Ends
	// after the model calls propose_plan. The PlanWorkflow reads PLAN.md next.
	PlanningMode bool `json:"planning_mode,omitempty"`
	// PlanHandling: a mid-plan follow-up turn under a PlanWorkflow — a normal
	// reason-act turn (it can answer the user and use tools) that ALSO gets the
	// `propose_plan` tool so it can reshape the still-pending plan tail given
	// what the follow-up asked. Not PlanningMode (which is propose-plan-only).
	PlanHandling bool `json:"plan_handling,omitempty"`
	// PlanID: the task-run this turn belongs to (the planning turn's id). Set by
	// PlanWorkflow for every turn it runs, and by dispatch.go for a Lite turn it
	// pre-resolved. When Task is also set, TurnWorkflow skips its own
	// ClassifyRequest.
	PlanID string `json:"plan_id,omitempty"`
	// Task: pre-resolved classification, passed through when PlanID is set.
	Task *TaskRepresentation `json:"task,omitempty"`
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
	// NeedsApproval — docs/components/request-pipeline/08-planning.md. Only a
	// planning turn sets it (from its one ModelCall's propose_plan). PlanWorkflow
	// reads it off the planning turn's result to decide whether to run the
	// approval gate. A control bool, not content.
	NeedsApproval bool `json:"needs_approval,omitempty"`
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
	TurnID string `json:"turn_id"`
	// PlanID — the task-run this turn belongs to (== the planning turn's id).
	// Prompt assembly reads the staged skills + the PLAN.md ledger by this, and
	// propose_plan / checkpoint_done updates apply against it. Empty for a Lite
	// or conversational turn.
	PlanID     string `json:"plan_id"`
	ContextSeq int    `json:"context_seq"`
	// PlanHandling — docs/components/request-pipeline/08-planning.md. A mid-plan
	// follow-up turn: normal reason-act, but `propose_plan` is offered alongside
	// the regular tools and peeled the same way PlanningMode peels it, so the
	// turn can revise the plan tail while still answering the user.
	PlanHandling bool `json:"plan_handling,omitempty"`
	// docs/components/model-registry.md, "Resolved: Selection Mechanism" —
	// the previous step's self-declared hint for this step, threaded
	// through opaquely (this workflow never interprets these, just copies
	// ModelCallOutput's hint fields into the next call's input). Empty on
	// the very first call of a turn — the Python side's
	// model_registry.default_hint() supplies {language, medium} in that
	// case, not a literal default here.
	HintModality string `json:"hint_modality"`
	HintTier     string `json:"hint_tier"`
	// Complexity — docs/components/request-pipeline/02-request-understanding.md.
	// Step 2's complexity estimate, threaded through opaquely so ModelCall can
	// bootstrap the turn's FIRST tier from it instead of always starting at
	// medium. The workflow never interprets it; empty for subagents and when
	// step 2 fell back.
	Complexity string `json:"complexity"`
	// PlanningMode — docs/components/request-pipeline/08-planning.md, Phase 3C.
	// This is the planning turn under a PlanWorkflow: ModelCall uses the
	// planning system prompt, offers only `propose_plan`, and peels that call
	// off to write PLAN.md. The turn ends after one such call.
	PlanningMode bool `json:"planning_mode,omitempty"`
}

// ClassifyRequestInput is ClassifyRequest's only input
// (docs/components/request-pipeline/02-request-understanding.md). The activity
// reads the turn's seed user message (and a little recent context) from
// Postgres itself and returns a TaskRepresentation.
type ClassifyRequestInput struct {
	TurnID string `json:"turn_id"`
}

// TaskRepresentation is step 2's output — small derived routing signals only:
// the two routing scalars (intent/complexity), the classifier's confidence, a
// distilled retrieval query, and a few named entities. Carried by the workflow
// the same way ModelCallOutput.NextHintTier and ToolCallRef's {Server,Tool}
// are — routing metadata derived from the message, not the message content
// (which stays Postgres-side). RetrievalQuery/Entities are passed straight
// into the step-4/5/7 retrieval activities by RetrievalWorkflow. Confidence ==
// A zero value marks an un-classified turn; ClassifyRequest fails rather than
// returning one (no fallback — request-pipeline/02-request-understanding.md).
type TaskRepresentation struct {
	Intent         string   `json:"intent"`     // "conversational" | "question" | "task" | "meta"
	Complexity     string   `json:"complexity"` // "trivial" | "simple" | "moderate" | "complex"
	Confidence     float64  `json:"confidence"`
	RetrievalQuery string   `json:"retrieval_query"`
	Entities       []string `json:"entities"`
	// ContinuesPrior — whether this message continues the session's in-progress
	// task-run or starts a new one. Consumed by ResolveOpenPlan, which
	// cross-checks it against embedding similarity when Confidence is low.
	ContinuesPrior bool `json:"continues_prior"`
}

// MemoryRetrieveInput is MemoryRetrieve's input
// (docs/components/request-pipeline/04-memory-retrieval.md). RetrievalQuery is
// the distilled query from step 2's TaskRepresentation — a small derived
// signal passed straight in, not read from Postgres.
//
// MemoryRetrieve runs once PER TURN, staged under the current turn's id
// (OwnerID = TurnID).
type MemoryRetrieveInput struct {
	OwnerID        string `json:"owner_id"` // the current turn_id — turn_retrieval staging key
	RetrievalQuery string `json:"retrieval_query"`
	// ParentTurnID is set only for a subagent turn ("Subagents are full
	// agents"). When present, MemoryRetrieve copies the parent turn's staged
	// kind='memory' rows instead of re-querying agent-brain — memory is about
	// the user's world, stable across a turn tree.
	ParentTurnID string `json:"parent_turn_id,omitempty"`
}

// ToolDiscoverInput is ToolDiscover's input
// (docs/components/request-pipeline/07-tool-discovery.md). REVISED 2026-09-02:
// runs once PER TURN, staged under OwnerID = the current turn_id.
type ToolDiscoverInput struct {
	OwnerID        string   `json:"owner_id"`
	RetrievalQuery string   `json:"retrieval_query"`
	Entities       []string `json:"entities"`
}

// SkillDiscoverInput is SkillDiscover's input
// (docs/components/request-pipeline/05-skill-discovery.md). Plan-scoped — runs
// once on the planning turn, staged under PlanID for the prompt + RecordSkill.
type SkillDiscoverInput struct {
	PlanID         string `json:"plan_id"`
	RetrievalQuery string `json:"retrieval_query"`
}

// RecordSkillInput is RecordSkill's input
// (docs/components/skill-subsystem.md REVISION 2026-09-02). Dispatched once when
// a task-run (PlanWorkflow, or a Deliberate subagent turn) finishes. The
// activity reads the whole multi-turn trajectory / tool calls / staged skill
// rows / PLAN.md from Postgres + the PV itself, then match-or-inserts against
// skill_procedures. Intent/Complexity/CloseReason come from the caller (there
// is no `episodes` row to read them from — decision B).
type RecordSkillInput struct {
	PlanID      string `json:"plan_id"`
	StopReason  string `json:"stop_reason"`
	Intent      string `json:"intent"`
	Complexity  string `json:"complexity"`
	CloseReason string `json:"close_reason"` // "plan_complete" | "superseded" | "turn_end" | ""
}

// --- docs/components/request-pipeline/08-planning.md — task-run resolution ---
//
// Decision B (episode-lifecycle.md): the PlanWorkflow *is* the task-run. There
// is no `episodes` table. `plan_id` == the anchor/planning turn id; a running
// PlanWorkflow has id "<plan_id>:plan".

// ResolveOpenPlanInput — dispatch.go asks: is there a Deliberate task already in
// progress for this session, and does this new message continue it?
type ResolveOpenPlanInput struct {
	SessionKey string             `json:"session_key"`
	TurnID     string             `json:"turn_id"` // the just-inserted message's turn
	Task       TaskRepresentation `json:"task"`
}

// ResolveOpenPlanResult:
//   - Continue: a PlanWorkflow is running for this session and this message
//     continues its task — the caller signals "<PlanID>:plan".
//   - Supersede: a PlanWorkflow is running but this is a new task — the caller
//     signals it to abandon, then starts a fresh plan.
//   - otherwise both false: no plan in progress.
type ResolveOpenPlanResult struct {
	PlanID        string `json:"plan_id"`
	ShouldContinue bool  `json:"should_continue"`
	Supersede     bool   `json:"supersede"`
}

// SubsystemResult is what each retrieval-phase activity returns to
// RoutingWorkflow — a status and the count of rows it staged to
// turn_retrieval. No content: the rows are read from turn_retrieval by later
// steps. Status is "ok" | "empty" | "error" from the activity; RoutingWorkflow
// records "timed_out" / "skipped" in the same shape for subsystems it didn't
// run or that missed the phase deadline.
type SubsystemResult struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

// ToolCallRef is one tool call minted by ModelCall — name/ID/dispatch-kind
// only, no arguments. The workflow uses this to decide Activity-vs-child-workflow
// dispatch; it never sees the arguments themselves.
type ToolCallRef struct {
	ToolCallID string `json:"tool_call_id"`
	ToolName   string `json:"tool_name"`
	IsSubagent bool   `json:"is_subagent"`
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

// ModelCallOutput is ModelCall's only output — refs and usage, never content
// or arguments.
type ModelCallOutput struct {
	HasToolCalls bool          `json:"has_tool_calls"`
	ToolCalls    []ToolCallRef `json:"tool_calls"`
	Usage        Usage         `json:"usage"`
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
	// docs/components/model-registry.md, "Resolved: Selection Mechanism" —
	// this step's self-declared hint for the next step, copied verbatim
	// into the next ModelCallInput. This workflow never interprets these.
	NextHintModality string `json:"next_hint_modality"`
	NextHintTier     string `json:"next_hint_tier"`
	// NeedsApproval — docs/components/request-pipeline/08-planning.md. Set on a
	// planning turn's one call when the model's `propose_plan` asked for
	// approval before execution. A control bool, same category as NextHintTier;
	// TurnWorkflow copies it into TurnResult and PlanWorkflow gates on it.
	NeedsApproval bool `json:"needs_approval,omitempty"`
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
	// PlanID — set on the is_turn_start call for any turn under a PlanWorkflow
	// (docs/components/request-pipeline/08-planning.md). Written to
	// turns.plan_id, which RecordSkill's trajectory gather keys on (by prefix,
	// so a nested plan's turns are swept in too).
	PlanID string `json:"plan_id,omitempty"`
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
	Kind          string            `json:"kind"` // "permission" | "decision" | ...
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

// --- docs/components/request-pipeline/08-planning.md — plan-and-execute ---

// PlanWorkflowInput starts a PlanWorkflow — the orchestrator for one Deliberate
// task-run (workflow id "<plan_id>:plan"). It runs the planning turn, gates on
// approval, then dispatches one checkpoint TurnWorkflow per non-terminal
// checkpoint in PLAN.md, folds in any mid-plan follow-up at each checkpoint
// boundary, and finally records the skill + tells the coordinator it's done.
type PlanWorkflowInput struct {
	PlanID       string             `json:"plan_id"` // == the planning turn id
	SessionKey   string             `json:"session_key"`
	ConnectionID string             `json:"connection_id,omitempty"`
	InitiatedBy  string             `json:"initiated_by,omitempty"`
	Task         TaskRepresentation `json:"task"`
	// --- 3C-iii checkpoint recursion (docs/components/request-pipeline/08-planning.md) ---
	// ParentPlanID: the plan whose complex checkpoint spawned this one. Empty ⟺
	// this IS the root. A nested plan's completion reaches its parent via the
	// child-workflow future — no PlanDone signal (that's root→coordinator only),
	// no RecordSkill of its own (the root's one RecordSkill prefix-sweeps every
	// turn in the tree, since each nested plan's turn ids sit under the root's).
	ParentPlanID string `json:"parent_plan_id,omitempty"`
	// Depth: 0 at the root, +1 per nesting level. A checkpoint at maxPlanDepth
	// runs as a flat turn instead of recursing (it can still spawn subagents).
	Depth int `json:"depth,omitempty"`
	// SeedText: the spawning checkpoint's seed message. Empty ⟺ root (its
	// planning turn is PreInserted by dispatch.go); set for a nested plan, whose
	// planning turn inserts this as its own seq-0 message.
	SeedText string `json:"seed_text,omitempty"`
}

// NextCheckpointResult — the NextCheckpoint activity reads PLAN.md and returns
// the first non-terminal checkpoint, formatted as the seed message text for a
// checkpoint TurnWorkflow (intent + done_when + the whole rendered plan for
// context + the "call checkpoint_done" instruction). HasNext is false when
// every checkpoint is terminal.
type NextCheckpointResult struct {
	HasNext      bool   `json:"has_next"`
	CheckpointID string `json:"checkpoint_id,omitempty"`
	SeedText     string `json:"seed_text,omitempty"`
	// Complex — the planning model flagged this checkpoint as itself a
	// multi-step subtask (propose_plan's per-checkpoint `complex`). PlanWorkflow
	// runs it as a nested PlanWorkflow instead of a flat turn (3C-iii), unless
	// the depth cap is hit.
	Complex bool `json:"complex,omitempty"`
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
