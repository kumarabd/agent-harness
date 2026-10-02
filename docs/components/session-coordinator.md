# Component: Session Coordinator

Implemented in `loop-worker/workflow/coordinator.go`. One workflow per session, addressed by session key, is the gateway's durable signal target.

## Responsibilities

- Start or signal one ordinary `TurnWorkflow` at a time — the only dispatch target there is.
- Keep FIFO user input, seed turn numbering with `GetMaxTurnSeq`, and preserve messages that race chat completion.
- Fold a proactive Wake into the active chat turn, or seed a new one if none is running.
- Forward the session Cancel signal into the active chat turn.
- Carry the turn-sequence counter through continue-as-new once the session is quiescent.

A 2026-09-22…29 conversational-skills/session-mode mechanism (user-selected
modes, a generic `skill_command` tool, a bounded `SkillStepWorkflow` child,
`ConversationState` query, `SkillCommand` Update) was built, iterated through
several production incidents, and removed outright on 2026-09-30, per
direction. There is no mode, skill, or secondary dispatch target of any kind
— every message goes through ordinary chat.

## Durability and idle behavior

No conversation content lives in the coordinator's own state — only the turn-sequence counter and the currently-running chat child, if any. Content is stored in Postgres.

With no active chat child and nothing queued, the coordinator idles out after thirty seconds and starts an abandoned `WriteMemoryWorkflow` before closing. A fresh `SignalWithStart` recreates it on demand.

KeepAlive resets idle waiting. The Cancel signal interrupts the active chat turn without any other side effect.

Chat children retain `ParentClosePolicy=ABANDON`. A worker process restart replays workflow history; deliberately terminating/replacing a coordinator is not an equivalent recovery path. There is no implemented cross-execution adoption of an abandoned chat child, and operators must not assume one.

## Verification

Coordinator tests cover ordinary dispatch, continuing a turn sequence across a restart, requeuing unprocessed messages after a turn, and folding a proactive wake into an active chat turn.
