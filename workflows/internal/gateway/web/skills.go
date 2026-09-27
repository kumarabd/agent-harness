package web

import (
	"encoding/json"
	"net/http"

	skillswf "agent-harness/workflows/internal/workflow/skills"
)

type skillSummary struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type listSkillsResponse struct {
	Skills []skillSummary `json:"skills"`
}

// handleListSkills serves workflows/internal/workflow/skills/registry.go's
// hand-maintained mirror of activities/activities/skills.py — a skill IS its
// own Temporal workflow (docs/05-architecture-domain-control-loops.md, "A
// Skill Is a Workflow, Not a Document"), so there is no separate "steps" or
// "prompt" field to show here: registry.go's InputSchema plus skills.py's
// own description are the whole of what a skill declares about itself from
// the outside. Same shape for every user — skills are process-wide, hand-
// authored, never per-tenant or per-session.
func (h *Handler) handleListSkills(w http.ResponseWriter, r *http.Request) {
	out := make([]skillSummary, len(skillswf.Registry))
	for i, s := range skillswf.Registry {
		out[i] = skillSummary{Name: s.Name, Description: s.Description, InputSchema: s.InputSchema}
	}
	writeJSON(w, http.StatusOK, listSkillsResponse{Skills: out})
}
