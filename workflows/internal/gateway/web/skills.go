package web

import (
	"encoding/json"
	"net/http"
)

type skillSummary struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type listSkillsResponse struct {
	Skills []skillSummary `json:"skills"`
}

// handleListSkills queries this tenant's own `skills` table (migration
// 040_skills.sql) directly — 2026-09-27, replacing a hand-maintained mirror
// of activities/activities/skills.py (workflows/internal/workflow/skills/
// registry.go, deleted alongside this). A skill IS its own Temporal workflow
// (docs/05-architecture-domain-control-loops.md, "A Skill Is a Workflow, Not
// a Document"), so there is no separate "steps" or "prompt" field to show
// here: this table's input_schema plus description are the whole of what a
// skill declares about itself from the outside.
//
// Same Postgres this tenant's own tenant-worker already reads via
// skills.init(pool) (activities/activities/skills.py) — no new cross-service
// plumbing, and `enabled` now actually varies per tenant instead of every
// tenant's image shipping the same hardcoded global list.
func (h *Handler) handleListSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rows, err := h.pool.Query(ctx,
		"SELECT name, description, input_schema FROM skills WHERE enabled ORDER BY name",
	)
	if err != nil {
		http.Error(w, "failed to list skills", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []skillSummary
	for rows.Next() {
		var s skillSummary
		var inputSchema []byte
		if err := rows.Scan(&s.Name, &s.Description, &inputSchema); err != nil {
			http.Error(w, "failed to list skills", http.StatusInternalServerError)
			return
		}
		s.InputSchema = json.RawMessage(inputSchema)
		out = append(out, s)
	}

	writeJSON(w, http.StatusOK, listSkillsResponse{Skills: out})
}
