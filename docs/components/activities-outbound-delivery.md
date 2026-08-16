# Component: Activities & Outbound Delivery

> STATUS: IN PROGRESS — cancellation, granularity, retry policy, and deliver-path idempotency are resolved. A handful of numeric/technology choices remain open.

### Role (one line)
The retryable, side-effecting units the workflow invokes — model call, tool calls, persist, and deliver.

### Responsibilities (from architecture)
- Model-call activity: the reasoning step (LLM inference).
- Tool-call activities: each tool call is its own retryable activity.
- Persist activity: write the assistant response + updated history to Postgres (durable record).
- Deliver activity: makes the actual outbound send call to the platform, dispatched via Temporal's own task-queue routing directly to the gateway worker that owns the target shard/platform — see below.
- **Cooperative cancellation:** every tool-call activity must be cancellable on request from its owning [[temporal-workflow|turn workflow]] when an interrupt arrives — see below.

### Key Design Decisions (recap)
- Tool calls and model calls as activities → retryable, durable, resumable.
- **Superseded:** delivery was originally planned via a message broker "outbound queue" (Redis Streams/NATS) between the deliver activity and the gateway (Option A, chosen over a direct send endpoint, Option B). **Revised in `components/gateway.md`:** the gateway itself runs an embedded Temporal worker for `DeliverActivity`, and the turn workflow targets it directly via a deterministically-computed task queue (by shard for connection-based platforms, by platform for webhook-based ones). No broker needed — Temporal's own activity dispatch provides the durability/retry/targeting that the broker was introduced for. See `components/gateway.md`'s "Resolved: Outbound Flow" section for the full reasoning.

### Resolved: Cooperative Cancellation Contract (for interrupt support)
When a turn workflow receives a mid-turn interrupt signal, it requests cancellation of whatever tool-call activities are currently in flight. `ABANDON` was explicitly rejected as the cancellation type — it lets the workflow move on instantly but leaves the activity running unobserved on the worker with an unknown final outcome (did the side effect happen or not?), which is worse than the latency it saves. Chosen instead:

- **Activity cancellation type: `WAIT_CANCELLATION_COMPLETED`.** The workflow blocks until the activity has actually acknowledged cancellation and torn itself down (or hit its own timeout) — always a known final state, never an orphan.
- **Every tool activity implementation must:**
  1. Call `heartbeat()` periodically (interval TBD per tool — see open questions).
  2. Check for a pending cancellation at each heartbeat / between chunks of work.
  3. On seeing one, actively tear down the underlying operation (kill the subprocess, abort the HTTP client, close the socket) — not just stop heartbeating and let it dangle.
- **This makes interrupt latency bounded, not instant** — worst case is one heartbeat interval plus the tool's own cleanup time. Intentional trade for never losing track of a cancelled activity's real outcome.
- **Atomic, non-chunkable tools are a residual exception.** A tool that's a single blocking call with no internal checkpoint (e.g. one webhook POST, one third-party SDK call with no cancel hook) cannot be interrupted mid-call under any mechanism — cooperative cancellation only has a chance to act *between* checkpoints. Such tools simply run to completion before the fold-in happens; this must be documented per-tool, not silently assumed solved by the general policy.
- **Subagents cancel via a different, already-native mechanism:** `RequestCancelChildWorkflowExecution` cascades down automatically to a running subagent child workflow, which applies this same heartbeat-cooperative pattern to whatever activity *it* currently has in flight, recursively. No extra plumbing needed beyond what child workflows already give us.
- **What the model sees:** a cancelled tool call must be surfaced as an explicit "cancelled, no result" observation — distinct from a normal tool error — so the model reasons about it correctly rather than treating a missing result as a failure. Exact shape resolved below.

### Resolved: Heartbeat Interval Policy
Not one global constant — the policy tiers by the tool's own duration/chunkability, declared as metadata at tool-registration time:

- **Tier A — fire-and-complete tools** (typically sub-2-second: a single API call, a file read, a small lookup). **No heartbeating implemented.** By the time a heartbeat could fire, the call is already done — cooperative cancellation would add implementation cost for no realistic benefit. These fall back to "runs to completion, then fold-in" — the same behavior as the atomic-tool residual case, not a separate exception to it. In practice, most simple tools land here.
- **Tier B — long-running, timer-chunkable tools** (shell exec, anything with unpredictable duration and no natural internal checkpoints). **Default heartbeat every 3 seconds**, on a background timer inside the activity, checking for cancellation at each tick. `heartbeat_timeout` set to ~3x the interval (~10s) — enough slack to tolerate a transient network blip without the server falsely declaring the activity dead, while still keeping worst-case interrupt latency (heartbeat interval + teardown time) on the order of a few seconds, which is negligible against a multi-second agent turn (same reasoning already applied to outbound-queue latency elsewhere in these docs).
- **Tier C — tools with natural checkpoints** (batch/streaming operations — processing N items, paginated fetches). Heartbeat **at each checkpoint** rather than on a fixed timer, since that's a more meaningful and cheaper signal than an arbitrary clock. Pair with a Tier-B-style timer as a floor if checkpoints could plausibly be far apart (e.g. one very large item), so cancellation detection doesn't stall waiting for a slow checkpoint.
- **Per-tool override:** the 3s/10s Tier B default is a starting point, not a hard rule — a specific tool can declare its own interval/timeout at registration if its profile warrants it. The *tiering itself* (A/B/C, chosen per tool at registration) is the resolved decision; the exact numeric defaults may get tuned once we have real tool latency profiles.

### Resolved: "Cancelled, No Result" Observation Shape
A cancelled tool call (or cancelled subagent — same shape, since a subagent looks like a tool call to its parent) is persisted and fed back to the model as a distinct status, not squeezed into the normal success/error result fields:

- `status: "cancelled"` — a third value alongside `"ok"` / `"error"`, so the model never confuses "interrupted" with "the tool tried and failed."
- `reason: "interrupted_by_new_message"` — makes clear *why*, so the model understands this wasn't a bug and doesn't need to retry the same call blindly; the right move is usually to address the new message first.
- `side_effect: "none" | "partial" | "unknown"` — reported by the tool's own teardown logic, not assumed by the framework. `WAIT_CANCELLATION_COMPLETED` guarantees we know the *activity's* final state (it acknowledged cancellation), but that is not automatically the same as guaranteeing the underlying side effect was cleanly rolled back — a killed subprocess might have already written partial output. Default to `"unknown"` unless a tool's teardown explicitly proves a clean abort; this keeps the honesty guarantee that motivated rejecting `ABANDON` in the first place — we don't trade "orphaned activity" for "falsely confident cancellation."
- `partial_output` (optional) — if the tool captured something useful before teardown (partial file content, partial API response), expose it rather than discarding it; the model may still find it useful context.
- **Delivered alongside the new message, not instead of it.** When the workflow resumes after cancellation, the reasoning step sees both the cancellation observation *and* the newly-arrived user message in the same context update — the interrupted action doesn't silently vanish from history.
- This becomes a first-class shape in the message/tool-call schema (relevant to the state-layer's persisted history — see `components/state-layer.md`), not a repurposed error field.

### Resolved: Activity Granularity
One activity per tool call — never batched — and this was mostly already implied by earlier decisions, just not stated outright:
- A single reasoning step's parallel tool calls are already independent concurrent activity futures. Batching several calls into one activity would break independent retry (a batch failure forces re-running calls that already succeeded), independent cancellation (`WAIT_CANCELLATION_COMPLETED` cancels *an* activity, not one call inside a bundle), and observability (Temporal's history is legible per-call, matching the workflow-ID tree scheme).
- One activity per model call (reasoning step) — unchanged.
- Persist and deliver remain their own activities, not folded into the tool-call loop.
- Subagents are child workflows, never activities — a different primitive; granularity doesn't apply to them.
- A tool call is atomic from Temporal's perspective. Heartbeating/cancellation happens *inside* the activity's own code (per the Tier A/B/C policy above); no need to decompose a tool call into sub-activities.

### Resolved: Retry Policy — Model-Driven, Not a Static Playbook
Initial framing considered a static per-activity-type retry table (fixed attempt counts, hardcoded error classification, one policy per tool). Rejected as unnecessarily rigid and unnecessary implementation burden — most retry judgment is exactly the kind of contextual decision the model already handles for cancelled/failed tool calls elsewhere in this design (surfaced as an observation, the model reasons about what to do next). Static classification code was solving a problem the loop can mostly solve on its own. The resolved shape instead splits retry into three tiers by *who* is positioned to make the decision safely:

1. **Transient-infra retry — mechanical, below the model, minimal.** A dropped connection, a 500, or a worker dying mid-call happens before the activity has returned anything — there's no observation yet for the model to reason about, so a small amount of retry has to live beneath that boundary regardless. Kept deliberately generic rather than a rich per-activity-type table: a couple of attempts with short exponential backoff, uniform across activities, using Temporal's standard `RetryPolicy`. This is the floor, not the design center.

2. **Logical/business failures — surfaced to the model, not auto-retried mechanically.** "File not found," a real 4xx, a content-policy refusal, an invalid argument — retrying identically will fail identically, and only the model has the context to decide whether to retry with different arguments, try a different tool, or give up. These are configured as `RetryPolicy.NonRetryableErrorTypes` so Temporal doesn't try to be "helpful" by silently re-running them, and surface immediately as a failed-tool-call observation — the same established pattern as cancelled calls and failed subagents.

3. **The model can request its own retry behavior when issuing a tool call.** Temporal supports setting `RetryPolicy` per activity *invocation*, not just per registered type — so when the model issues a tool call, it can pass a retry hint (e.g. "this endpoint is flaky, worth a few attempts with backoff" vs. "don't retry, just report failure") and the workflow forwards that straight into the activity's start options. This is where "LLMs are quite good as the judge" actually applies safely — the model tunes retry *behavior*, not the one thing it structurally cannot know (see next).

**The one thing that must stay a static, mechanical ceiling regardless of model judgment: side-effect idempotency.** Whether a call is safe to retry after an ambiguous failure (timeout, dropped connection after the request was sent) depends on whether the underlying side effect already happened server-side — and that fact is not recoverable from the error itself. A timed-out payment call and a cleanly-rejected one can look identical to the caller. No amount of model reasoning conjures that fact from nothing; only the tool (via an idempotency key, or an explicit "check if it already happened" step) can know it. So every tool declares a static idempotency class at registration, alongside its heartbeat tier:
- **Idempotent / safely-repeatable** (read-only, or a write that's naturally idempotent, e.g. upsert-by-key): retry is unconstrained — the model's requested attempt count is honored as-is.
- **Non-idempotent side effect** (send email, payment, anything with an external fire-once semantic): hard ceiling of **`MaximumAttempts: 1`** by default, regardless of what the model requests. The model's retry hint is *clamped* by this ceiling, never allowed to override it. A tool can only raise its own ceiling by proving its own idempotency key internally — that's a per-tool decision made at registration, not something the model can grant itself mid-turn. On failure past the ceiling, this surfaces as an observation with `side_effect: "unknown"` (the same shape already defined for cancellation above), so the model is told honestly that the outcome is uncertain rather than either silently retried or silently dropped.

**A hard overall attempt cap still exists as a backstop**, independent of any of the above — a cap, not a policy, so a confused model can't retry a failing call indefinitely and burn cost/latency with nothing stopping it.

**Timeouts reuse the heartbeat tiers already defined, rather than a separate scheme:** Tier A tools (no heartbeat) rely on a short `StartToCloseTimeout` as their only failure detector. Tier B/C tools rely on `HeartbeatTimeout` (already sized ~3x the heartbeat interval) as the real liveness signal, paired with a longer or open-ended `StartToCloseTimeout` — the heartbeat, not the outer timeout, is what's actually watching for a stuck activity.

**Net effect:** exhausted retries and non-retryable logical errors both resolve the same way — a failed-tool-call observation handed back to the model, consistent with everything else already decided. The static piece shrinks to exactly the part that has to be static (idempotency ceiling, minimal transient-retry floor, overall attempt cap); everything else — what counts as worth retrying, with what arguments, how many times within the ceiling — is the model's call, made fresh each time through the loop rather than baked into a fixed playbook.

### Resolved: Deliver Idempotency Key
The response produced by a turn already has a natural, free identity — the **turn workflow ID** (`{session_key}:turn:{n}`), since one turn produces exactly one final response. No new ID scheme needed; `response_id = turn_workflow_id`.

Two independent places can produce a duplicate, and the key has to survive both:
- **Temporal retrying the deliver activity itself** — a transient failure before the activity acknowledges completion causes a normal Temporal activity retry.
- **The gateway crashing between "sent to the platform API" and "the activity call returning"** — Temporal would see the activity as not-yet-completed and dispatch it again to whichever worker (possibly a replacement) is polling that shard's/platform's task queue.

`response_id` flows through persist → deliver → the activity call → the gateway's send path. The load-bearing mechanism is the **gateway keeping its own idempotency ledger**: a small Postgres table (`delivered_responses(response_id, delivered_at)`), checked-then-inserted immediately before the actual platform send call. This is what makes the real, user-visible side effect (the platform API call) safe regardless of how many times the activity gets redispatched — the standard pattern for turning an at-least-once dispatch into an effectively-once visible action. (Previously framed around a message-broker "outbound queue" — superseded by the task-queue-based delivery design in `components/gateway.md`; the ledger mechanism itself is unchanged.)

### Resolved: Tool-Registration Metadata Shape
Heartbeat tier and idempotency class are orthogonal (one governs cancellation detection, the other governs retry safety) and are declared together, independently, at tool registration:

```
{
  name: "send_email",
  heartbeat:   { tier: "A" },
  idempotency: { class: "non_idempotent" },
}
{
  name: "web_search",
  heartbeat:   { tier: "A" },
  idempotency: { class: "idempotent" },
}
{
  name: "shell_exec",
  heartbeat:   { tier: "B", intervalSeconds: 3, timeoutSeconds: 10 },
  idempotency: { class: "non_idempotent" },  // conservative default — see caveat
}
```

**Known limitation, no fix planned — this is standard Temporal practice, not a gap to close.** Generic tools like `shell_exec` (or a raw SQL runner, a script executor) aren't really *one* idempotency class — `ls` is trivially safe to retry, `rm -rf` is not. A single static per-tool-type classification is too coarse for genuinely polymorphic tools, and it's tempting to reach for a smarter per-invocation classifier (e.g. the model self-declaring idempotency per call, checked against a destructive-pattern denylist). That was considered and explicitly rejected as overcomplicating a problem that's mostly self-inflicted by our own architecture: harnesses that execute tools in-process and synchronously (Claude Code, Codex, Cursor) don't face this at all, since there's no network hop between "command ran" and "result observed" for a response to go missing across — the ambiguous-failure case barely exists for them. Idiomatic Temporal guidance for an activity that isn't provably idempotent is exactly the blunt version: don't try to infer safety at retry time, just cap it at `MaximumAttempts: 1` and surface the failure. So `shell_exec` (and anything similarly polymorphic) simply defaults to `non_idempotent` — full stop, no per-invocation refinement planned. The cost (losing auto-retry on plenty of harmless commands) is accepted in exchange for not building a bespoke classification mechanism nothing else in this space uses.

Destructive-command *safety* (as opposed to retry-safety) is a related but genuinely separate concern — the standard pattern for that across other harnesses is permission-gating before execution (allow/deny policy, optionally a human prompt, checked before the command runs at all), not anything to do with retries after the fact. Not designed here — see `future-work.md`.

### Resolved: Where the Model's Retry Hint Lives
Not inside a tool's own argument schema — retry behavior is infrastructure, not business logic, and folding it into every tool's JSON schema would mean redeclaring it per tool. Instead it's a generic, optional sibling field on the **tool-call envelope** itself — part of the harness's tool-calling protocol, documented once, applying uniformly to every tool call:

```
{
  tool: "web_search",
  arguments: { query: "..." },
  retry_hint: { max_attempts: 3, strategy: "backoff" }  // optional
}
```

Omittable on every call; when absent, the tool's own registered default retry policy applies. This keeps the common case (most tool calls) free of any extra fields, and only surfaces when the model has an actual reason to override — consistent with `retry_hint` being clamped by, never overriding, the tool's static idempotency ceiling.

### Resolved: Panic Handling — Defense-in-Depth, Not Load-Bearing
**Introduced 2026-08-14**, alongside the reference-passing contract in `components/temporal-workflow.md`. Every content-touching activity (`ModelCall`, `ToolCall`) should catch and sanitize its own panics rather than letting a raw exception (with its message, stack trace, or captured local variables) propagate to the workflow — good practice regardless of anything else.

**What this is not:** the primary mechanism protecting tenant content from a shared loop-worker pool. Under the reference-passing contract, activities never hand the workflow raw content on the success path — the dominant path — so a panic has structurally little left to leak even if unhandled. Panic suppression closes the one residual seam (a serialization failure surfacing content-bearing *input* in a Temporal-generated failure, if inputs aren't reference-only) rather than being what makes sharing the loop-worker pool safe in the first place. Recorded here explicitly so this isn't mistaken for the load-bearing isolation mechanism it was floated as before the reference-passing design existed.

### Open Questions / To Design
- Exact task-queue naming scheme for deliver dispatch — see `components/gateway.md`.
- Exact numeric heartbeat interval/timeout tuning once real tool latency profiles exist (tiering itself is resolved — see above).
- Exact value of the overall attempt cap (backstop, independent of the model's per-call retry requests).
- Exact reference/ID shape `ModelCall` and `ToolCall` read and return under the reference-passing contract (`components/temporal-workflow.md`) — contract is resolved, literal field shape is not.

### Notes Log
- 2026-08-06: Resolved the interrupt-cancellation mechanism — cooperative cancellation (`WAIT_CANCELLATION_COMPLETED` + heartbeats) is the contract every tool activity must implement; `ABANDON` explicitly rejected. See `02-architecture-temporal-execution.md` §3 and [[temporal-workflow]].
- 2026-08-06: Resolved heartbeat interval policy (tiered by tool duration/chunkability: A=none, B=3s timer, C=checkpoint-based) and the cancelled-tool-call observation shape (`status`/`reason`/`side_effect`/`partial_output`, delivered alongside the new message).
- 2026-08-07: Resolved activity granularity (one activity per tool call, confirming what was already implied) and retry policy. Rejected a static per-activity-type retry table in favor of a model-driven approach: minimal generic transient-retry floor below the model, logical failures surfaced as observations for the model to reason about (not auto-retried), and the model can pass a per-invocation retry hint via Temporal's per-call `RetryPolicy`. The one static ceiling that can't be delegated to the model: side-effect idempotency class per tool (`MaximumAttempts: 1` default for non-idempotent tools, clamping any model request), because whether a side effect already happened isn't recoverable from the error itself. Plus a hard overall attempt cap as a backstop.
- 2026-08-07: Closed out the three remaining threads from the retry-policy work: deliver idempotency uses the turn workflow ID as `response_id`, enforced by a gateway-side Postgres ledger (`delivered_responses`) checked before the actual platform send; tool-registration metadata declares heartbeat tier and idempotency class as independent fields; the model's retry hint lives as an optional `retry_hint` field on the tool-call envelope, not inside per-tool argument schemas.
- 2026-08-07: Considered and rejected a model-proposed per-call idempotency classifier (with a static destructive-pattern denylist as a clamp) for polymorphic tools like `shell_exec`. Rejected as overcomplicating a problem that's largely specific to our own distributed-activity architecture — in-process harnesses (Claude Code, Codex, Cursor) don't face this failure mode at all. Reverted to the plain, standard-Temporal-practice answer: polymorphic tools default to `non_idempotent`, no per-invocation refinement planned. Destructive-command *safety* (distinct from retry-safety) reframed as a future permission-gating item — see `future-work.md`.
- 2026-08-07: Superseded the "outbound queue" (message broker) framing for delivery — resolved in `components/gateway.md` as task-queue-based dispatch directly to a gateway-embedded Temporal worker instead. The `delivered_responses` idempotency mechanism is unchanged, just now guards against activity redispatch rather than broker redelivery. Dropped the now-resolved "outbound queue technology choice" and "how deliver addresses the owning gateway" open questions accordingly.
- 2026-08-14: Resolved panic handling as defense-in-depth: activities should still catch/sanitize their own panics, but this is not what makes the shared-workflow-worker-pool decision in `components/multi-tenancy.md` safe — the reference-passing contract in `components/temporal-workflow.md` is, since it keeps content out of the workflow's memory on the success path regardless of panic handling.
- 2026-08-15: Implemented the first real tool, `shell_exec`, per this doc's resolved Tier B policy exactly (heartbeat ~3s, timeout ~10s) and `non_idempotent` with no per-command classifier, as resolved. `activities/activities/tool_call.py` (previously stub-only, no error path at all) now dispatches by tool name through a registry (`tools.py`) and produces a real `status='error'` result for both handler exceptions (sanitized to just the message, per this doc's panic-handling note) and unrecognized tool names. The workflow side needed a small, previously-missing piece to make per-tool heartbeat tiers actually reach `ActivityOptions`: `workflows/internal/workflow/turn.go` hardcoded one timeout for every tool call regardless of tier; added a small per-tool lookup (`tool_tiers.go`, hand-mirrored from the Python registry same as `types.go`/`ids.go` already are) so only `shell_exec` gets real Tier B numbers, with existing fixture-only demo tools falling back to their prior fast local-demo timing unchanged. Verified end-to-end against real Postgres/Temporal, including real cooperative-cancellation teardown of an actual subprocess (and its whole process group, not just the shell) — see `components/session-filesystem.md`'s 2026-08-15 note for a real teardown bug this surfaced and fixed.
