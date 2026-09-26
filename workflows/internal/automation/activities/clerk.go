package activities

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// CreateClerkOrganization creates a Clerk Organization for this new tenant
// and sets the requesting user as its initial member (Clerk's own
// created_by semantics — the org's first admin) — the concrete mechanism
// behind "tenant identity is Clerk Organization membership"
// (docs/components/gateway/web.md's "Resolved: Shared agent-web +
// Identity-Routing Router"). Plain net/http against Clerk's Backend REST
// API, no SDK — same "avoid an SDK dependency that assumes more than this
// actually needs" discipline workflows/internal/gateway/clerkauth's own doc
// comment already applied when it dropped clerk-sdk-go.
//
// a.ClerkSecretKey is the FIRST secret key anywhere in this codebase — every
// other Clerk integration so far (clerkauth.VerifyJWT, this router's own
// JWT verification) deliberately needs only the project's public JWKS,
// specifically to avoid provisioning one. Creating an Organization is a
// genuinely privileged, write-side operation Clerk's public JWKS can't
// authorize; this activity is the one legitimate reason to hold it, and it
// never leaves this activity (not logged, not part of this activity's own
// return value beyond the resulting org id, which is not itself secret).
func (a *Activities) CreateClerkOrganization(ctx context.Context, ref PublicRef) (string, error) {
	var orgID string
	err := runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		if a.ClerkSecretKey == "" {
			return fmt.Errorf("CLERK_SECRET_KEY is not configured on the automation worker")
		}
		base := a.ClerkAPIBaseURL
		if base == "" {
			base = "https://api.clerk.com"
		}

		body, err := json.Marshal(map[string]any{
			"name":       ref.TenantSlug,
			"slug":       ref.TenantSlug,
			"created_by": ref.RequesterUserID,
		})
		if err != nil {
			return fmt.Errorf("marshal clerk organization request: %w", err)
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/organizations", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.ClerkSecretKey)
		req.Header.Set("Content-Type", "application/json")

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return fmt.Errorf("clerk create organization: %w", err)
		}
		defer res.Body.Close()
		respBody, _ := io.ReadAll(res.Body)
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return fmt.Errorf("clerk create organization: status %d: %s", res.StatusCode, string(respBody))
		}

		var parsed struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return fmt.Errorf("parse clerk organization response: %w", err)
		}
		if parsed.ID == "" {
			return fmt.Errorf("clerk create organization: response had no id")
		}
		orgID = parsed.ID
		return nil
	})
	return orgID, err
}
