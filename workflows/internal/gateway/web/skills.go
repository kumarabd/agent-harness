package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"agent-harness/workflows/internal/tenantid"
	"github.com/jackc/pgx/v5"
)

type skillSummary struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Enabled     bool            `json:"enabled"`
}

type listSkillsResponse struct {
	Skills []skillSummary `json:"skills"`
}

// handleListSkills reads all tenant skill selections. Domain behavior lives
// in the Python skills registry; every user message still enters ordinary chat.
// input_schema remains catalog metadata, not a directly callable skill schema.
func (h *Handler) handleListSkills(w http.ResponseWriter, r *http.Request) {
	if !h.authorizedForSkills(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx := r.Context()
	rows, err := h.pool.Query(ctx,
		"SELECT name, description, input_schema, enabled FROM skills ORDER BY name",
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
		if err := rows.Scan(&s.Name, &s.Description, &inputSchema, &s.Enabled); err != nil {
			http.Error(w, "failed to list skills", http.StatusInternalServerError)
			return
		}
		s.InputSchema = json.RawMessage(inputSchema)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "failed to list skills", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, listSkillsResponse{Skills: out})
}

func (h *Handler) authorizedForSkills(r *http.Request) bool {
	userID := userIDFromContext(r.Context())
	return userID != "" && h.tenantSlug != "" && tenantid.SlugForSub(userID) == h.tenantSlug
}

func (h *Handler) handleSetSkillEnabled(w http.ResponseWriter, r *http.Request) {
	if !h.authorizedForSkills(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	name := r.PathValue("name")
	if name == "" {
		http.Error(w, "skill name is required", http.StatusBadRequest)
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || req.Enabled == nil {
		http.Error(w, "enabled must be a boolean", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var skill skillSummary
	var inputSchema []byte
	err := h.pool.QueryRow(r.Context(),
		"UPDATE skills SET enabled = $1, updated_at = now() WHERE name = $2 "+
			"RETURNING name, description, input_schema, enabled",
		*req.Enabled, name,
	).Scan(&skill.Name, &skill.Description, &inputSchema, &skill.Enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "skill not found", http.StatusNotFound)
		return
	}
	if err != nil {
		http.Error(w, "failed to update skill", http.StatusInternalServerError)
		return
	}
	skill.InputSchema = json.RawMessage(inputSchema)
	writeJSON(w, http.StatusOK, skill)
}
