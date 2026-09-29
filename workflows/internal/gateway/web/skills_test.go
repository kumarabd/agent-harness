package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func skillRequest(method, body, userID string) *http.Request {
	req := httptest.NewRequest(method, "/skills/journaling", strings.NewReader(body))
	req.SetPathValue("name", "journaling")
	return req.WithContext(context.WithValue(req.Context(), clerkUserIDKey, userID))
}

func TestSkillEndpointsRejectOtherTenant(t *testing.T) {
	h := &Handler{tenantSlug: "user-alice"}
	for _, method := range []string{http.MethodGet, http.MethodPatch} {
		recorder := httptest.NewRecorder()
		req := skillRequest(method, `{"enabled":true}`, "user_bob")
		if method == http.MethodGet {
			h.handleListSkills(recorder, req)
		} else {
			h.handleSetSkillEnabled(recorder, req)
		}
		if recorder.Code != http.StatusForbidden {
			t.Errorf("%s: got status %d, want 403", method, recorder.Code)
		}
	}
}

func TestSetSkillEnabledRejectsInvalidBodies(t *testing.T) {
	h := &Handler{tenantSlug: "user-alice"}
	for _, body := range []string{
		`{}`, `{"enabled":null}`, `{"enabled":"true"}`,
		`{"enabled":true,"other":1}`, `{"enabled":true} {"enabled":false}`,
	} {
		recorder := httptest.NewRecorder()
		h.handleSetSkillEnabled(recorder, skillRequest(http.MethodPatch, body, "user_alice"))
		if recorder.Code != http.StatusBadRequest {
			t.Errorf("body %q: got status %d, want 400", body, recorder.Code)
		}
	}
}
