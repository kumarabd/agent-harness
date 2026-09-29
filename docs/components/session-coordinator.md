# Component: Session Coordinator

Implemented in `workflows/internal/workflow/coordinator.go`. One workflow per session, addressed by session key, is the gateway's durable signal target and the owner of conversational skill state.

## Responsibilities

- Load user-selected mode from the session row on cold start; apply only explicit authenticated user selection commands.
- Start or signal one ordinary `TurnWorkflow` at a time. Never dispatch a mode-specific conversation workflow.
- Keep FIFO user input, seed turn numbering with `GetMaxTurnSeq`, and preserve messages that race chat completion.
- Expose `ConversationState` through a query and serialize explicit `SkillCommand` Updates with user selection.
- Own one independent bounded skill child. Chat interruption leaves it running; explicit cancel or mode change requests cancellation.
- Queue skill outcomes for ordinary chat after the active turn finishes. Skills never deliver directly.
- Carry state through continue-as-new only when children, update handlers and input queues are clear.

See [Conversational skills](../05-architecture-domain-control-loops.md) for commands, confirmation, retry semantics and rollout.

## Durability and idle behavior

Mode is durable user configuration in `sessions.mode`, attributed to the selecting message. Proposal references, revision and lifecycle phase live in Temporal workflow state. Content is stored in Postgres; no database task/event ledger drives execution.

The latest proposal/outcome remains available through the coordinator across idle periods. User selection changes clear an inactive proposal and advance the revision. A running old step is cancelled and its terminal outcome is still reported. With no proposal or active work, the coordinator idles out after thirty seconds and starts an abandoned `WriteMemoryWorkflow` before closing.

KeepAlive resets idle waiting. Wake retains intention provenance and enters ordinary chat. The Cancel signal interrupts chat and any running skill without changing selection.

Chat children retain `ParentClosePolicy=ABANDON`; skill children use `REQUEST_CANCEL` and wait for cooperative cancellation. A worker process restart replays workflow history; deliberately terminating/replacing a coordinator is not an equivalent recovery path. There is no implemented cross-execution adoption of an abandoned chat child, and operators must not assume one.

## Verification

Coordinator tests cover ordinary dispatch under skill selection, independent skill/chat execution, pending and terminal snapshot retention, explicit cancellation and deferred notices. Production-history replay and live delivery validation are separate rollout checks. Old open histories must be drained with the old build before this breaking coordinator rewrite is deployed.
