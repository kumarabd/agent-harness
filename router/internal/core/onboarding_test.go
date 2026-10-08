package core

import (
	"net/http/httptest"
	"strings"
	"testing"

	"agent-harness/shared/clerkauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/mocks"
)

func TestOnboardingRepeatedSubmissionCannotRestartCompletedWorkflow(t *testing.T) {
	issuer := newTokenIssuer(t, "onboarding-idempotency")
	temporal := &mocks.Client{}
	slug := TenantForSub("user_2abc", 8090, 8080).Slug
	options := mock.MatchedBy(func(o client.StartWorkflowOptions) bool {
		return o.ID == workflowIDForSlug(slug) && o.TaskQueue == "system" &&
			o.WorkflowIDReusePolicy == enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE_FAILED_ONLY
	})
	args := []any{mock.Anything, workflowIDForSlug(slug), "start", nil, options, mock.Anything, mock.Anything}
	temporal.On("SignalWithStartWorkflow", args...).Return(nil, nil).Once()
	temporal.On("SignalWithStartWorkflow", args...).Return(nil, &serviceerror.WorkflowExecutionAlreadyStarted{}).Once()
	handler := New(clerkauth.Config{JWKSURL: issuer.jwks.URL}, 8090, 8080).WithOnboarding(temporal, "system").Handler()
	for range 2 {
		req := httptest.NewRequest("POST", "/onboard", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+issuer.token(t, issuer.key, "user_2abc"))
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		assert.Equal(t, 202, rec.Code)
		assert.JSONEq(t, `{"status":"accepted"}`, rec.Body.String())
	}
	temporal.AssertExpectations(t)
}

func TestOnboardingStillReportsTemporalFailure(t *testing.T) {
	issuer := newTokenIssuer(t, "onboarding-unavailable")
	temporal := &mocks.Client{}
	temporal.On("SignalWithStartWorkflow", mock.Anything, mock.Anything, "start", nil, mock.Anything, mock.Anything, mock.Anything).
		Return(nil, &serviceerror.Unavailable{Message: "offline"}).Once()
	handler := New(clerkauth.Config{JWKSURL: issuer.jwks.URL}, 8090, 8080).WithOnboarding(temporal, "system").Handler()
	req := httptest.NewRequest("POST", "/onboard", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+issuer.token(t, issuer.key, "user_2abc"))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	assert.Equal(t, 500, rec.Code)
	temporal.AssertExpectations(t)
}
