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

// handleListSkills reads tenant-enabled skill selections. Domain behavior lives
// in the Python skills registry; every user message still enters ordinary chat.
// input_schema remains catalog metadata, not a directly callable skill schema.
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
