package activities

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"google.golang.org/protobuf/types/known/durationpb"
)

// RegisterTemporalNamespace is the first place in this codebase that
// creates a Temporal namespace programmatically — every namespace before
// this (including this worker's own "system" namespace) was created by a
// human running `temporal operator namespace create <name>`
// (deploy/helm/agent-harness-shared/values.yaml's own comment,
// docs/components/multi-tenancy.md). Uses client.NewNamespaceClient — a
// separate, short-lived client (NOT the regular workflow/activity Client
// this worker itself runs on, which is bound to the "system" namespace and
// has no namespace-admin API), dialed fresh per call since namespace
// creation is rare enough that pooling one long-lived NamespaceClient for
// the worker's whole lifetime isn't worth the complexity.
func (a *Activities) RegisterTemporalNamespace(ctx context.Context, ref PublicRef) error {
	return runStep(ctx, a.Store, ref.RequestID, func(ctx context.Context) error {
		nsClient, err := client.NewNamespaceClient(client.Options{HostPort: a.TemporalAddress})
		if err != nil {
			return fmt.Errorf("dial namespace client: %w", err)
		}
		defer nsClient.Close()

		retentionDays := a.NamespaceRetentionDays
		if retentionDays <= 0 {
			retentionDays = 30 // same default every other real namespace in this cluster uses today, per multi-tenancy.md's own onboarding runbook
		}

		err = nsClient.Register(ctx, &workflowservice.RegisterNamespaceRequest{
			Namespace:                        ref.TenantSlug,
			Description:                      fmt.Sprintf("agent-harness tenant %q, self-serve onboarded", ref.TenantSlug),
			OwnerEmail:                       "",
			WorkflowExecutionRetentionPeriod: durationpb.New(time.Duration(retentionDays) * 24 * time.Hour),
		})
		if err != nil {
			var alreadyExists *serviceerror.NamespaceAlreadyExists
			if errors.As(err, &alreadyExists) {
				// Idempotent: a workflow retry (or a re-submitted onboarding
				// request after a partial earlier failure) landing here a
				// second time is not itself an error — the namespace this
				// step wants to exist already does.
				return nil
			}
			return fmt.Errorf("register temporal namespace %q: %w", ref.TenantSlug, err)
		}
		return nil
	})
}
