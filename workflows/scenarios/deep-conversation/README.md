# Deep Conversation Suite

A full-stack validation suite, not just loop mechanics. Where `../` covers the
original implementation slice (subagents, interrupts, parallel tool calls,
max-iterations — all deterministic, scripted, free), this suite specifically
targets everything built *since*: memory writes, session-wide context
assembly, context compression, model-tier hint declaration, tool discovery
via mcp-hub/shell-hub, the coordinator's turn_seq fix, and the failTurn
silent-drop fix. Several of these only exercise meaningfully against a real
model — a scripted response has no `next_step_hint` to declare, no memory
worth writing, nothing to compress. So this suite mixes scripted (cheap,
deterministic, reused directly from `../`) and real (costs actual API calls)
turns, and is explicit about which is which.

## Structure

**Main chained session** (one `-session`, run in this order, real wall-clock
gaps between some steps matter — see `run.sh`):

| Step | File | Scripted or real | Validates |
|---|---|---|---|
| 1 | `../shell-exec-basic.json` | scripted | Basic tool-calling still works at session open. |
| 2 | `../shell-exec-parallel.json` | scripted | Two tool calls targeting the same path genuinely serialize via `session_filesystem_leases` (not simulated). |
| 3 | `../subagent-spawn.json` | scripted | Recursive child Turn Workflow + merge-back into parent context. Scripted because there is currently no real tool the model can call to spawn a subagent — `is_subagent` is hardcoded `false` for every real tool call in `llm.py`. Worth knowing: subagent spawning is untestable via a real model today. |
| 4 | `04-remember-fact.json` | **real** | A real `ModelCall` → `WriteMemoryWorkflow` (detached child) actually fires and writes to agent-brain. |
| 5 | `05-recall-fact.json` | **real** | Run after a deliberate 35s+ gap (past `idleTTL`=30s) so the Session Coordinator has actually restarted. Validates two things in one step: (a) `lcm.assemble`'s session-wide context correctly recalls the fact from step 4 purely from Postgres, and (b) the `GetMaxTurnSeq` fix — this turn must mint `turn:5`, not collide back onto `turn:1`. |
| 6 | `06-tool-search.json` | **real** | `search_tools` genuinely fans out to mcp-hub (real backends: GitHub, Exa, Finance, Grafana, ABRP) and shell-hub. Deliberately asks the model *not* to call anything found — avoids taking a real, possibly-write action against a real external API with real credentials. |
| 7 | `07a`..`07g-compress-*.json` | **real** | Seven substantive real turns, run with `LANGUAGE_MEDIUM_CONTEXT_WINDOW` temporarily lowered (see `run.sh`) so `CompressContextWorkflow` genuinely fires within a short conversation instead of needing dozens of turns against the real 128K default. Also implicitly exercises `declare_next_step_hint` and tier escalation on every one of these real calls. |
| 8 | `08-post-compress-check.json` | **real** | Asks the model to summarize the conversation and recall the step-4 fact — validates that `assemble()` correctly picked the compacted `context_summaries` row back up, not just that compaction happened. |

**Isolated scenarios** (`isolated/`, each its own `-session`, not chained into
the above — each either deliberately exhausts a stop condition or deliberately
breaks something, so folding them into the main conversation would corrupt
it):

| File | Scripted or real | Validates |
|---|---|---|
| `../interrupt-initial.json` + `../interrupt-followup.json` | scripted | Real cooperative cancellation + signal coalescing. |
| `../max-iterations.json` | scripted | Loop halts exactly at `max_iterations=20`. |
| `isolated/tool-call-retry.json` | scripted | A tool call naming an unregistered tool genuinely produces `status='error'` (`tool_call.py`'s real "unknown tool" path, not simulated) and `turn.go`'s `retries` counter increments — without exhausting `max_retries=5`. |
| `isolated/induced-failure.json` | **real, but against a deliberately broken endpoint** | `ModelCall` exhausts `RetryPolicy.MaximumAttempts=3` (escalate-on-retry, fast→medium→expert, all genuinely attempted) → `failTurn` fires → `turns.status='failed'`, a synthetic error message lands in `messages`, `Deliver` sends it. Run with `PIONEER_BASE_URL` pointed at `http://127.0.0.1:1` (see `run.sh`) — real failures, zero API cost. |

## What this suite does *not* cover

- Level 2/3 compaction escalation (LLM aggressive-bullets, deterministic
  truncate) — Level 1 (LLM preserve-detail) has always succeeded in every
  live test so far; forcing a Level 1 failure on purpose isn't scripted here.
- Leaf→condensed summary folding (`LEAF_FOLD_THRESHOLD=5` leaf summaries) —
  step 7 is sized to cross the soft/hard token threshold at least once, not
  necessarily to produce 5 separate leaf summaries.
- `merge_subagent_output` — not implemented yet (`components/session-filesystem.md`).
- Claim-check large-payload routing through the PV — not implemented yet.
- Real gateway inbound path — `starter` is still the stand-in; no real
  Discord/Slack channel involved.

## Running it

See `run.sh` — it handles port-forwards (Temporal, Postgres, the LiteLLM
embedding endpoint, mcp-hub), scaling the live cluster Deployments to 0 so
local binaries are the only consumers, the temporarily-lowered context
window, and the induced-failure endpoint swap. Verification (Postgres/Temporal
ground-truth checks) is manual — see the printed instructions at the end of
the run.
