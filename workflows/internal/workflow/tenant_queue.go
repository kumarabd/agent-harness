package workflow

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
)

// saTenantSlug — a Temporal Search Attribute (not a Postgres column) tagging
// every tenant-scoped workflow execution (CoordinatorWorkflow, TurnWorkflow,
// IntentionWorkflow, ...) with its owning tenant, so `temporal workflow
// list`/the Temporal Web UI can filter "show me tenant X's workflows"
// directly — a real capability lost the moment every tenant started sharing
// one Temporal namespace (2026-09-26) instead of getting its own (workflow
// IDs themselves — session keys, turn IDs — don't embed the tenant slug
// anywhere). Same pattern intention.go's own IntentionUser/IntentionKind/
// IntentionState already use, and the same one-time deploy step: MUST be
// registered on the namespace before use (`temporal operator
// search-attribute create --name TenantSlug --type Keyword --namespace
// agents`) — see docs/components/multi-tenancy.md's search-attribute list.
// Set via WithTenantTaskQueue below, not a separate call — best-effort
// (errors swallowed, same tolerance intention.go's own upserts get): a
// missing/unregistered attribute means ops loses a filtering convenience,
// never a functional break.
var saTenantSlug = temporal.NewSearchAttributeKeyKeyword("TenantSlug")

// TenantActivityQueue returns the task queue a tenant's own tenant-worker
// fleet polls for activities (ModelCall, ToolCall, InsertMessage, Persist,
// WriteMemory, CompressContext, FireIntention, ...) —
// docs/components/multi-tenancy.md's "Resolved: Shared Temporal Namespace,
// Per-Tenant Task Queues" (2026-09-26). Every tenant now shares ONE Temporal
// namespace (replacing one namespace per tenant), so this is the entire
// isolation mechanism left on the Temporal side for these activities: get it
// wrong and one tenant's ModelCall could be picked up by another tenant's
// tenant-worker, which holds that OTHER tenant's own Postgres/LLM
// credentials. Computed the same way everywhere (Go and the tenant chart's
// own configmap.yaml template) so it can never drift.
func TenantActivityQueue(tenantSlug string) string {
	return tenantSlug + "-loop"
}

// WithTenantTaskQueue marks ctx so every activity this workflow execution
// dispatches routes to that tenant's own queue by default, AND tags the
// execution with the TenantSlug search attribute (see saTenantSlug above) —
// bundled into one call specifically so a future new workflow entry point
// can't get the queue routing right while forgetting the observability
// tagging (or vice versa); the two concerns are unrelated in *purpose* but
// travel together at every real call site. Call once, at the top of any
// independently-started workflow entry point (CoordinatorWorkflow,
// TurnWorkflow, WriteMemoryWorkflow, CompressContextWorkflow,
// UserInputRequestWorkflow, IntentionWorkflow, the skill workflows) — every
// plain Go function call made downstream within that SAME execution (the
// reason-act loop, failTurn, notifyProgress, ...) inherits the queue-routing
// half automatically, the same way workflow.WithActivityOptions' own
// timeout/retry settings would. It does NOT cross a child-workflow-start
// boundary — each of those entry points receives its own TenantSlug field
// on its input and must call this itself; a child does not inherit its
// parent's context values or search attributes.
//
// Queue-routing relies on a specific, verified property of the Temporal Go
// SDK: workflow.WithActivityOptions only overwrites TaskQueue when the
// caller explicitly sets a non-empty one (internal/workflow.go's
// WithActivityOptions: `if len(options.TaskQueue) > 0 { eap.TaskQueueName =
// options.TaskQueue }`). So every existing ActivityOptions{...} call site in
// this package that leaves TaskQueue unset keeps working unchanged and
// picks up the tenant queue automatically; the delivery-family call sites,
// which already set an explicit per-connection TaskQueue
// (deliveryTaskQueue et al.), are unaffected — those routes were never
// namespace-dependent to begin with (a Discord connection id is already
// globally unique).
func WithTenantTaskQueue(ctx workflow.Context, tenantSlug string) workflow.Context {
	ctx = workflow.WithTaskQueue(ctx, TenantActivityQueue(tenantSlug))
	_ = workflow.UpsertTypedSearchAttributes(ctx, saTenantSlug.ValueSet(tenantSlug))
	return ctx
}
