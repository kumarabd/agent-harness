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
// 040_skills.sql) directly. A "skill" is entirely a session mode now
// (docs/05-architecture-domain-control-loops.md, 2026-09-27 — entered via
// tools.switch_mode, not called directly by name): this row's `enabled` is
// this tenant's own opt-in/opt-out gate on a mode name, mirrored into
// activities/activities/llm.py's ENABLED_MODES. input_schema is unused for
// a mode row (switch_mode has its own single schema, not per-row) — kept
// in the response only because the column itself still exists on the row.
//
// Same Postgres this tenant's own tenant-worker already reads via
// skills.init(pool) (activities/activities/skills.py) — no new cross-service
// plumbing.
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
