package types

// ConversationState is coordinator-owned durable control state. Content stays
// in Postgres; queries expose references plus the state chat needs to route input.
type ConversationState struct {
	Mode  string     `json:"mode"`
	Skill SkillState `json:"skill"`
}

type SkillState struct {
	Kind      string `json:"kind"`
	ContentID string `json:"content_id"`
	StepID    string `json:"step_id"`
	Revision  int    `json:"revision"`
	Phase     string `json:"phase"`
}

type SkillCommandInput struct {
	ToolCallID string            `json:"tool_call_id"`
	SessionKey string            `json:"session_key"`
	State      ConversationState `json:"state"`
}

type SkillCommand struct {
	Action         string `json:"action"`
	ContentID      string `json:"content_id"`
	AlreadyApplied bool   `json:"already_applied"`
}

type SkillStepInput struct {
	StepID     string `json:"step_id"`
	ContentID  string `json:"content_id"`
	Kind       string `json:"kind"`
	TenantSlug string `json:"tenant_slug"`
	SessionKey string `json:"session_key"`
}

type SkillIteration struct {
	StepID    string `json:"step_id"`
	ContentID string `json:"content_id"`
	Kind      string `json:"kind"`
	Index     int    `json:"index"`
}

type SkillDecision struct {
	HasCall  bool `json:"has_call"`
	Mutation bool `json:"mutation"`
}

type SkillObservation struct {
	Complete bool `json:"complete"`
}

type UserSelectionInput struct {
	SessionKey string  `json:"session_key"`
	Message    Message `json:"message"`
}
