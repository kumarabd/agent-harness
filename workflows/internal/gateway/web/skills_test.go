package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	skillswf "agent-harness/workflows/internal/workflow/skills"
)

func TestHandleListSkillsReturnsRegistry(t *testing.T) {
	h := &Handler{}
	req := httptest.NewRequest(http.MethodGet, "/skills", nil)
	rec := httptest.NewRecorder()

	h.handleListSkills(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got listSkillsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(got.Skills) != len(skillswf.Registry) {
		t.Fatalf("got %d skills, want %d", len(got.Skills), len(skillswf.Registry))
	}
	for i, s := range got.Skills {
		want := skillswf.Registry[i]
		if s.Name != want.Name {
			t.Errorf("skill %d: name = %q, want %q", i, s.Name, want.Name)
		}
		if s.Description != want.Description {
			t.Errorf("skill %d (%s): description mismatch", i, s.Name)
		}
		var gotSchema, wantSchema map[string]any
		if err := json.Unmarshal(s.InputSchema, &gotSchema); err != nil {
			t.Fatalf("skill %d (%s): input_schema is not valid JSON: %v", i, s.Name, err)
		}
		if err := json.Unmarshal(want.InputSchema, &wantSchema); err != nil {
			t.Fatalf("registry entry %d (%s) has invalid input_schema JSON: %v", i, want.Name, err)
		}
	}
}
