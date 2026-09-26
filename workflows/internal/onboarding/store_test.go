package onboarding

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Regression test for a real bug: Step originally had no JSON tags, so
// GET /onboard/{request_id} (workflows/internal/router/core/onboarding.go,
// which serializes a []Step slice directly) sent capitalized Go field
// names instead of the lowercase ones agent-web's src/lib/onboarding.ts
// OnboardingStep type expects — silently breaking the whole progress view.
func TestStepJSONFieldNames(t *testing.T) {
	step := Step{Step: "HelmInstallTenant", Status: StepStatusDone, Message: "ok", UpdatedAt: time.Unix(0, 0).UTC()}
	raw, err := json.Marshal(step)
	require.NoError(t, err)

	var decoded map[string]any
	require.NoError(t, json.Unmarshal(raw, &decoded))
	require.Contains(t, decoded, "step")
	require.Contains(t, decoded, "status")
	require.Contains(t, decoded, "message")
	require.Contains(t, decoded, "updated_at")
	require.NotContains(t, decoded, "Step")
	require.NotContains(t, decoded, "Status")
}
