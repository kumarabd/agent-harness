# Conversational skills and Temporal-owned control

Implemented design, 2026-09-28. This supersedes mode-specific top-level turns and the earlier directly callable skill workflows.

## One conversational front door

Every utterance enters ordinary `TurnWorkflow`, including diary content, confirmations, status questions, amendments, and unrelated conversation. The coordinator starts or signals exactly one chat child at a time. Neither selection nor a running skill replaces chat's system prompt or voice-delivery path.

The user chooses a durable session mode: `/skill journaling`, `/skill service_monitoring`, `/chat`, or a registered explicit spoken start/stop phrase (for example, “let's journal”). Selection commands must occupy the whole message; a topic mention is not a selection. Disabled skills are rejected. Successful completion, a status question, and ordinary conversation do not change the selected mode.

Chat reads the coordinator's `ConversationState` query on each real model call. It contains selection, proposal revision/reference, step reference and phase. Chat routes relevant content through the generic `skill_command` tool and handles unrelated/status questions conversationally. Domain prompts and tools are not injected into chat.

## Ownership

| Owner | Responsibility |
|---|---|
| User | Select/deactivate a skill, approve exact proposals, request cancellation |
| Coordinator workflow | Order messages and commands; own revision, phase, children and cancellation |
| Ordinary chat turn | Interpret incoming speech/text, issue explicit commands, request confirmation, persist and deliver replies |
| Bounded skill child | Advance approved domain work; never consume raw conversation or deliver replies |
| Skill definition | Domain prompt, allowed tools, argument validation, native handlers and verified completion criterion |
| Postgres | Session selection, immutable proposals, existing conversation/tool audit and provider I/O content |
| Temporal | All execution lifecycle, durable waits, child results, retries, cancellation and history |

There is no database task queue, event log, lease-based skill scheduler, or polling orchestration ledger. `skill_content` stores approved-domain content; `skill_io` stores checkpointed provider requests/responses, keyed by step and iteration. Neither table contains lifecycle state or schedules work.

## Command and confirmation contract

The top-level chat calls `skill_command(action, revision, content?)`. Its activity invokes the coordinator's `SkillCommand` Update with the existing tool-call ID as the idempotency key. The coordinator serializes updates and user selection with a workflow mutex.

- `submit` / `amend`: create an immutable proposal reference and advance the revision to `awaiting_confirmation`. An active step must first be cancelled and finish.
- `confirm`: require an exact-proposal confirmation request, then start one bounded child; return the new snapshot without blocking chat on domain work.
- `cancel`: require an explicit user cancellation request and cancel pending/running work without changing selection. Completed work cannot be undone this way.

The activity validates session ownership, top-level caller, live enablement, revision and a real user source. System notifications cannot authorize work. Selection changes advance the revision, preventing old commands from matching a newly selected skill.

Chat presents `ask_user(skill_content_id=...)`. The server substitutes the exact stored proposal and Approve / Do not proceed options; the model cannot replace the approval text. Confirmation is tied to that content ID. A real approval answer, or a later exact affirmative utterance following a pending/cancelled request, can confirm it. A stale revision, absent prompt, denial, status question or old “yes” cannot. The existing durable user-input child handles the wait and interruption.

Temporal update IDs deduplicate same-run retries. The existing tool-call result records applied commands for later duplicate detection. Provider decisions and responses are checkpointed so activity retries reuse committed content.

## Execution, interruption and delivery

`SkillStepWorkflow` is a coordinator-owned sibling of chat, not a child of a particular chat turn. Ordinary chat interruption never cancels it. Explicit cancellation, mode changes, or the session Cancel signal request cancellation. Cancellation cannot roll back an external write that has already happened.

Each step has at most twelve tool rounds and a ten-minute child timeout. Each round reasons with the skill's prompt and its offered tools, then executes one selected operation. Tool identity, domain arguments and the tenant permission policy are checked at execution. Permission-listed external tools fail closed; this implementation does not create a second approval UI inside the skill.

Reads can retry three times. Mutations have one attempt because an external provider may have committed a write before a network failure. Ambiguous writes are not automatically retried or reported as successful. Checkpointing is not a claim of exactly-once external side effects. Further work needs inspection and a newly confirmed proposal.

Only domain completion evidence can mark a step complete. A model stopping without a tool, reaching the budget, or making an unsupported call fails the step. A completed/cancelled/failed step queues a system notice for ordinary chat; that notice waits behind active chat and cannot interrupt a user confirmation. Existing turn persistence, ordered delivery and per-message voice behavior remain the user-facing path.

The coordinator retains proposal and terminal-result references across idle periods and worker replay. Continue-as-new carries that state when there are no active children, handlers or queued inputs. A session with no proposal can idle out after thirty seconds; a cold coordinator reloads user selection from `sessions.mode`. Deliberately terminating a workflow is not equivalent to restarting a worker and may discard its lifecycle state.

## Adding a skill

Register a `SkillDefinition` under `activities/activities/skills/` with a stable name, descriptions/selection aliases, domain prompt, exact external read/write allowlists, native handlers, argument validator and completion predicate. Import its registration in the package initializer and add its tenant catalog row/enablement migration. Domain-specific code stays in that module; no coordinator switch or new top-level turn type is needed.

The current definitions are [journaling](skills/journaling.md) and [service monitoring](skills/service-monitoring.md). This runtime does not give skills arbitrary shell, subagent or direct-delivery capabilities.

## Rollout and verification

Migration 044 adds selection attribution and the two content/audit tables, and refreshes catalog descriptions without changing tenant enablement. The SQL is mirrored in the tenant Helm chart.

This is a breaking workflow-code change: legacy `SetMode`, `switch_mode` and mode-specific workflow registrations are removed, not kept as compatibility dispatch. Drain old coordinators/turns with the old worker build before replacing workers, apply migration 044, then start the matching Go/Python workers. Do not replay old open workflow histories against this new implementation without an explicit migration/versioning strategy. No deployment is performed by this change.

Run `go test ./...` and `go vet ./...` in `workflows/`, and `PYTHONPATH=activities python -m unittest discover -s activities/tests` at the repository root. Tests cover conversational routing, independent execution, cancellation, deferred completion notices, exact approval, revisions, tenant gating, retry/checkpoint behavior and domain completion evidence. Live Notion/Grafana/voice integration and production-history replay remain deployment validation work.
