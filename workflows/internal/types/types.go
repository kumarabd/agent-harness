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

// ModelCallInput is ModelCall's only input — no content. ModelCall reads
// prior turn history from Postgres itself (it *is* the context-hydration
// step now) and looks up ContextSeq's scripted/real response.
type ModelCallInput struct {
	TurnID string `json:"turn_id"`
	// EpisodeID — docs/components/episode-lifecycle.md. The episode this turn
	// belongs to (== the anchor turn_id). Prompt assembly reads the staged
	// retrieval + plan ledger by this, and plan_progress updates apply against
	// it, so a continuation turn advances the episode's one ledger. Empty for a
	// conversational fast-path turn.
	EpisodeID  string `json:"episode_id"`
	ContextSeq int    `json:"context_seq"`
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
// 0 marks a fallback / un-classified turn (the activity degrades to a neutral
// representation rather than failing).
type TaskRepresentation struct {
	Intent         string   `json:"intent"`     // "conversational" | "question" | "task" | "meta"
	Complexity     string   `json:"complexity"` // "trivial" | "simple" | "moderate" | "complex"
	Confidence     float64  `json:"confidence"`
	RetrievalQuery string   `json:"retrieval_query"`
	Entities       []string `json:"entities"`
	// ContinuesPrior — docs/components/episode-lifecycle.md. Whether this
	// message continues the session's currently-open episode or starts a new
	// one. Only meaningful when an episode is open; false on the classifier
	// fallback (OpenEpisode's degraded path uses embedding similarity instead).
	ContinuesPrior bool `json:"continues_prior"`
}

// MemoryRetrieveInput is MemoryRetrieve's input
// (docs/components/request-pipeline/04-memory-retrieval.md). RetrievalQuery is
// the distilled query from step 2's TaskRepresentation — a small derived
// signal passed straight in by RoutingWorkflow, not read from Postgres.
type MemoryRetrieveInput struct {
	// EpisodeID (docs/components/episode-lifecycle.md) is the staging key —
	// rows go to turn_retrieval keyed by it. TurnID is the current turn, used
	// only for the reconcile-mode "latest user message" lookup.
	EpisodeID      string `json:"episode_id"`
	TurnID         string `json:"turn_id"`
	RetrievalQuery string `json:"retrieval_query"`
	// ParentTurnID is set only for a subagent turn ("Subagents are full
	// agents"). When present, MemoryRetrieve resolves the parent turn's episode
	// and copies its staged kind='memory' rows instead of re-querying
	// agent-brain — memory is about the user's world, stable across a turn tree.
	ParentTurnID string `json:"parent_turn_id,omitempty"`
	// Reconcile (episode-lifecycle.md / request-pipeline/08-planning.md) — when
	// true the activity re-keys on the current turn's latest user message and
	// replaces the episode's staged rows.
	Reconcile bool `json:"reconcile,omitempty"`
}

// ToolDiscoverInput is ToolDiscover's input
// (docs/components/request-pipeline/07-tool-discovery.md). Runs once per
// episode (the opening turn); staged by EpisodeID.
type ToolDiscoverInput struct {
	EpisodeID      string   `json:"episode_id"`
	RetrievalQuery string   `json:"retrieval_query"`
	Entities       []string `json:"entities"`
}

// SkillDiscoverInput is SkillDiscover's input
// (docs/components/request-pipeline/05-skill-discovery.md). See
// MemoryRetrieveInput for the EpisodeID / TurnID split.
type SkillDiscoverInput struct {
	EpisodeID      string `json:"episode_id"`
	TurnID         string `json:"turn_id"`
	RetrievalQuery string `json:"retrieval_query"`
	Reconcile      bool   `json:"reconcile,omitempty"`
}

// ComposeSkillInput is ComposeSkill's input
// (docs/components/request-pipeline/06-skill-composition.md). The activity
// reads the staged memory/tool/skill rows from turn_retrieval by EpisodeID.
type ComposeSkillInput struct {
	EpisodeID string `json:"episode_id"`
	// request-pipeline/08-planning.md — a reconcile-mode compose regenerates the
	// kind='composed' block but does not re-seed turn_plan.
	Reconcile bool `json:"reconcile,omitempty"`
}

// RecordSkillOutcomeInput is RecordSkillOutcome's input
// (docs/components/skill-subsystem.md, "Recording" + episode-lifecycle.md).
// Fires ONCE when an episode closes. The activity reads the whole multi-turn
// trajectory / tool calls / staged skill rows / plan ledger / episode row
// from Postgres itself.
type RecordSkillOutcomeInput struct {
	EpisodeID  string `json:"episode_id"`
	StopReason string `json:"stop_reason"`
}

// --- docs/components/episode-lifecycle.md ---

// OpenEpisodeInput — the activity reads the turn's parent_type / seed message /
// session from Postgres; the workflow supplies the turn_id and step 2's task
// representation.
type OpenEpisodeInput struct {
	TurnID string             `json:"turn_id"`
	Task   TaskRepresentation `json:"task"`
}

// OpenEpisodeResult — EpisodeID is what to stage/key the turn under. Attached
// means this turn joined an already-open episode (skip the full pipeline,
// reconcile-refresh only). SupersededEpisodeID is non-empty when a
// previously-open episode was closed to make room.
type OpenEpisodeResult struct {
	EpisodeID           string `json:"episode_id"`
	Attached            bool   `json:"attached"`
	SupersededEpisodeID string `json:"superseded_episode_id"`
}

// CompleteEpisodeInput — called at every turn end for a turn that belongs to an
// episode: records the turn's stop_reason on the episode and, if the plan
// ledger is now all-terminal, closes the episode as complete.
type CompleteEpisodeInput struct {
	EpisodeID  string `json:"episode_id"`
	StopReason string `json:"stop_reason"`
}

type CompleteEpisodeResult struct {
	Completed bool `json:"completed"`
}

// CloseSessionEpisodesInput — dispatched on the coordinator's idle-exit.
type CloseSessionEpisodesInput struct {
	SessionKey string `json:"session_key"`
}

type CloseSessionEpisodesResult struct {
	EpisodeIDs []string `json:"episode_ids"`
}

// SkillSynthesizeInput is SkillSynthesize's input
// (docs/components/skill-subsystem.md, "Synthesis"). The activity processes
// the whole un-synthesized candidate queue; this carries only the triggering
// turn, for logging.
type SkillSynthesizeInput struct {
	TriggerTurnID string `json:"trigger_turn_id"`
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
	// The resolved {server, tool} identity behind this call — "shell" +
	// the reduced command-name token for shell_exec, or the real
	// {server,tool} pair for call_tool. Crosses the reference-passing
	// boundary the same way ToolName already does (dispatch/routing
	// metadata, not the call's actual arguments — components/multi-tenancy.md's
	// "tool name... is workflow-visible by design, not an accepted leak").
	// Only populated when RequiresApproval is true; empty otherwise.
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
