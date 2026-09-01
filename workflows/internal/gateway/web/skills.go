package web

import (
	"encoding/json"
	"net/http"
	"time"
)

// skillProcedure is one current row of skill_procedures (docs/components/
// skill-subsystem.md) — the harness's learned/authored procedural memory.
// jsonb columns pass through as raw JSON; the browser renders them.
type skillProcedure struct {
	ID            string          `json:"id"`
	Version       int             `json:"version"`
	Title         string          `json:"title"`
	TriggerText   string          `json:"trigger_text"`
	Body          json.RawMessage `json:"body"`
	Preconditions json.RawMessage `json:"preconditions"`
	DoneCriteria  json.RawMessage `json:"done_criteria"`
	Notes         json.RawMessage `json:"notes"`
	Provenance    string          `json:"provenance"`
	Scope         string          `json:"scope"`
	SourceIDs     json.RawMessage `json:"source_ids"`
	Confidence    float64         `json:"confidence"`
	RunCount      int             `json:"run_count"`
	ClusterRadius *float64        `json:"cluster_radius"`
	LastUsedAt    *string         `json:"last_used_at"`
	CreatedAt     string          `json:"created_at"`
	UpdatedAt     string          `json:"updated_at"`
}

type listSkillsResponse struct {
	Skills []skillProcedure `json:"skills"`
}

// handleListSkills — a read-only view of what the agent currently knows how to
// do. Only current rows (valid_to IS NULL); no version history. The skill store
// is tenant-global (not per-user or per-session), so unlike the other handlers
// in this file there is no channel_id / user_id filter — every authenticated
// user of this tenant sees the same list. Ordered so the most-trusted,
// most-exercised procedures read first.
func (h *Handler) handleListSkills(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	rows, err := h.pool.Query(ctx,
		`SELECT id, version, title, trigger_text, body, preconditions, done_criteria,
		        notes, provenance, scope, source_ids, confidence, run_count,
		        cluster_radius, last_used_at, created_at, updated_at
		   FROM skill_procedures
		  WHERE valid_to IS NULL
		  ORDER BY confidence DESC, run_count DESC, title ASC`,
	)
	if err != nil {
		http.Error(w, "failed to list skills", http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	skills := make([]skillProcedure, 0)
	for rows.Next() {
		var s skillProcedure
		var clusterRadius *float64
		var lastUsedAt *time.Time
		var createdAt, updatedAt time.Time
		if err := rows.Scan(
			&s.ID, &s.Version, &s.Title, &s.TriggerText, &s.Body, &s.Preconditions,
			&s.DoneCriteria, &s.Notes, &s.Provenance, &s.Scope, &s.SourceIDs,
			&s.Confidence, &s.RunCount, &clusterRadius, &lastUsedAt, &createdAt, &updatedAt,
		); err != nil {
			http.Error(w, "failed to read skills", http.StatusInternalServerError)
			return
		}
		s.ClusterRadius = clusterRadius
		if lastUsedAt != nil {
			iso := lastUsedAt.Format(time.RFC3339)
			s.LastUsedAt = &iso
		}
		s.CreatedAt = createdAt.Format(time.RFC3339)
		s.UpdatedAt = updatedAt.Format(time.RFC3339)
		skills = append(skills, s)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, "failed to read skills", http.StatusInternalServerError)
		return
	}

	writeJSON(w, http.StatusOK, listSkillsResponse{Skills: skills})
}
