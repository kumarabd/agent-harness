# Component: Delegated Agents

> STATUS: SCAFFOLD — designed here for the first time, not yet implemented. The mechanism is fully specified below; what's new is the actual adapters (one per delegated CLI), the Postgres audit table, and the tool-registry wiring. Introduced 2026-08-28 alongside `docs/04-architecture-orchestrator-vision.md` and `components/skills.md`. Reflects the "leverage, don't rebuild" vision explicitly: delegate load-bearing work (coding, cloud ops, git ops) to specialist CLIs that already do it well, keep this harness's contribution focused on the orchestration itself.

### Role (one line)
The mechanism for spawning a specialist CLI (Claude Code, Codex, Aider, gh, aws, ...) as a subprocess, listening to its structured event stream to extract inner tool calls / files touched / cost, persisting those events to Postgres for real audit and cost accounting, and returning a clean summary to the orchestrator's turn — turning "opaque shell delegation" into "genuinely observable orchestration."

### Why this exists (recap)
Two separately real gaps, both articulated in `docs/04-architecture-orchestrator-vision.md`:

1. **The vision explicitly relies on delegation.** For coding tasks the harness delegates to Claude Code / Codex / Aider rather than building its own `read_file`/`edit_file`/etc. Without a real delegation mechanism, that's `shell_exec claude-code "..."` and back to opaque stdout. This component makes delegation first-class.
2. **Structured event streams already exist for every major coding CLI.** Claude Code has `--output-format stream-json` (real, documented). Codex has `codex exec --json`. Aider has `--stream`. gh has `--json` (non-streaming, still parseable). These aren't hypothetical — leveraging them turns a delegated call from a black box into something with real observability: inner tool calls, files touched, cost, exit reason.

Without this component, the orchestrator vision has a real problem: cost is invisible ("did that Claude Code call just spend $5 or $0.50?"), audit is impossible ("what files did it touch?"), failures are opaque ("did it error, or give up, or run out of budget?"). With it, all three become real information the orchestrator can reason about, log, meter, and (eventually) budget against.

### Responsibilities (from architecture)
- **One tool per delegated CLI** — `delegate_claude_code`, `delegate_codex`, `delegate_aider`, etc. Not a generic "spawn any CLI" tool. Each adapter knows the specific CLI's flags, event format, exit codes, cost field.
- **Spawn the CLI as a real subprocess in the calling turn/subagent's session directory** — same session-filesystem semantics as `shell_exec`. Real cancellation via process-group teardown (inherits `shell_exec`'s already-built machinery, since the tool sits in the same activity handler shape).
- **Stream + parse events line-by-line** while the subprocess runs. Persist every event to a new `delegated_agent_events` Postgres table for audit.
- **Extract a structured summary** at exit — final message, cost, files touched, exit reason — return to the orchestrator; don't dump the raw event stream into context.
- **Contribute to metrics** — per-delegation cost, duration, exit-status labels feed `budget-guardrails.md`'s metrics surface with a real `cost_usd` dimension for delegated calls.

### Key Design Decisions (recap)
- **One adapter per CLI, not a generic streaming shell.** `shell_exec` stays generic; `delegate_*` tools are per-CLI. Each adapter is small (parse a handful of fields), thin (don't try to model the full event grammar), and testable in isolation.
- **Structured summary returned to the orchestrator, not raw stdout.** The event stream is high-volume and mostly not useful to the orchestrator's own reasoning — it's audit data. Postgres captures it; the model sees a clean summary.
- **Every event to Postgres, always.** Audit doesn't get to be optional. A per-turn cost that says "$4.87" without any way to answer "on what?" is worse than none at all.
- **Same session-filesystem semantics as `shell_exec`.** Same working directory, same lease, same claim-check for large outputs. Delegated CLIs write files in the session dir the same way `shell_exec` runs commands there. No new filesystem primitives.
- **Same permission-gating shape.** `delegate_claude_code` is a `{server="delegate", tool="claude_code"}` identity for `permissions.py`'s existing `PERMISSION_LIST` — a tenant can require approval before spawning Claude Code without any new gating mechanism.

### Resolved: Return a Structured Summary, Not the Raw Stream
Three shapes considered:
- **(a) Parse the stream, return a structured summary.** Adapter collects events, extracts what matters, returns `{final_message, cost_usd, tool_call_count, files_touched, exit_status}`. Streaming is used *inside* the tool but the tool itself is one-shot to the orchestrator.
- **(b) Stream events out to the orchestrator's turn in real time via signals.** Each parsed event becomes a workflow signal, same mechanism `ModelCallChunk` uses. Powerful — orchestrator sees delegation unfold live and can react mid-flight — but expensive (real signal traffic per inner tool call the delegated agent makes).
- **(c) Both — Postgres for audit + structured summary to the model.** Middle ground.

**Landed on (c)**: Postgres receives every event (audit), the model receives a structured summary at exit (small, focused). Signals-to-workflow is genuinely useful only when the orchestrator has a concrete reason to react mid-delegation (e.g. interrupt if the delegation is going badly) — no such use case exists yet, and adding it later is additive, not a rework. Same reasoning as the model-registry work: build the mechanism to observe *now*, add feedback loops when a real need for them shows up.

### Resolved: One Adapter Per CLI, Not a Generic "Stream Any Tool"
Considered: a generic `delegate_agent(cmd, event_parser_name)` where the parser is registered separately, letting a new CLI be a config change instead of a code change. Rejected — each CLI's event grammar is genuinely different enough (Claude Code's discriminated union, Codex's flat token events, gh's non-streaming JSON) that a "generic" adapter would either be lowest-common-denominator (only extracts fields all CLIs share, which is basically nothing) or genuinely per-CLI with a config-driven parser that's harder to reason about than plain code. Adapters are small enough (~100 lines each) that keeping them as real Python code, not config, matches the same reasoning `providers/openai_provider.py` and `providers/anthropic_provider.py` already use — one file per shape, real code, easy to reason about, easy to test against a real CLI invocation.

### Resolved: New Postgres Table `delegated_agent_events`
Deliberately its own table, not folded into `tool_calls.result` (would explode a single JSON blob past reasonable size) or into `messages` (semantically distinct — this is audit data, not conversation content). Shape:

```
delegated_agent_events (
  tool_call_id  text NOT NULL REFERENCES tool_calls(tool_call_id),
  seq           int  NOT NULL,        -- ordering within one delegated call
  event_type    text NOT NULL,        -- "assistant_message", "tool_use", "tool_result", "usage", "cost", "error", ...
  event_json    jsonb NOT NULL,       -- the parsed event, adapter-specific shape
  created_at    timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (tool_call_id, seq)
);
CREATE INDEX ON delegated_agent_events (tool_call_id);
```

- `tool_call_id` FKs to `tool_calls` — each `delegate_*` call is a normal tool_call row with additional events attached, not a parallel universe.
- `event_type` is adapter-specific (each adapter documents its own vocabulary); Postgres doesn't enforce a whitelist — a new adapter defining a new type doesn't need a schema change.
- `event_json` carries whatever the adapter extracted (typed subset of the CLI's own event; not the raw CLI event verbatim, to insulate against upstream format churn — see "Adapter maintenance" below).
- No cross-tenant leakage risk — same per-tenant `agent_harness` database as everything else, same isolation as `messages`/`tool_calls`.

### Resolved: Failure Semantics Are Sharper Than `shell_exec`'s
Today `shell_exec` returns `{exit_code, stdout, stderr}`. A `delegate_*` adapter can distinguish:
- **CLI succeeded, delegated agent produced a genuine result** — normal `ok` return, summary populated.
- **CLI succeeded, delegated agent gave up** ("I couldn't figure out how to do X") — `ok` return, but summary includes an `agent_gave_up: true` field derived from the specific CLI's give-up-signal (Claude Code emits a specific final event shape when the model refuses; adapter recognizes it).
- **CLI ran but had a tool error partway through** — `ok` return; summary's `tool_call_count` and audit events tell the story.
- **CLI itself crashed / auth failed** — `error` return, summary carries the exit code and last-known stderr fragment.

That's genuinely better than parsing stdout for "sorry, I can't" style strings — same "surface honestly, don't silently paper over" reasoning `merge_subagent_output`'s `skipped_conflicts` shape uses.

### Resolved: Timing Is Tier B, But Longer
`shell_exec` is Tier B at 5-minute `StartToCloseTimeout`. A real Claude Code run on a real repo can be 10–15 minutes. `delegate_*` tools need:
- `HeartbeatTimeout`: same as `shell_exec` (10s) — the subprocess still emits events, we can heartbeat every N events or every N seconds.
- `StartToCloseTimeout`: 30 minutes (a real, generous ceiling — a Claude Code refactor of a nontrivial module can genuinely take that long; anything longer should probably be broken into subagents).
- Same cooperative-cancellation contract — heartbeats keep flowing while the subprocess runs; a cancellation signal cleanly kills the process group, closes the event stream reader, and marks the tool call cancelled.

### Considered and Rejected: Native `read_file` / `edit_file` / `fetch`
Explicitly dropped from the roadmap (see `docs/04-architecture-orchestrator-vision.md`). Delegation to Claude Code / Codex / Aider covers file operations at higher quality than any native tool this project could reasonably ship. This component is what makes that delegation good; native tools would compete with rather than complement it.

### Risks (Named, Not Glossed Over)
Each of these is real; each has a mitigation named in the doc rather than left implicit.

1. **Delegated tools are slow.** 30 seconds to 10+ minutes per call. Mitigation: longer `StartToCloseTimeout`, real cancellation, subagent-spawn for parallelism.
2. **Adapter maintenance burden.** Claude Code changes its `stream-json` shape → adapter breaks. Mitigations: (a) version-pin the delegated CLI in the tenant-worker image (already true implicitly; make it explicit); (b) keep adapters thin — parse a handful of fields only, don't model the whole event grammar; (c) fail loud on unknown event types, don't silently drop them (unknown types write to `delegated_agent_events` with `event_type="unknown"` and the raw payload, so nothing is lost); (d) each adapter has an integration test that runs the real CLI once during image build to catch breaking changes at build time, not first use.
3. **Auth / credential explosion.** Every CLI wants its own credentials — `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `GH_TOKEN`, `AWS_*`. **Accepted per direct user direction** (2026-08-28): "credentials for the CLI is fine, there are ways to handle them." Concretely: each credential is a discrete secret in the tenant's own Helm overrides (`deploy/helm/tenants/<tenant>.yaml`), same convention as `agentBrain.apiKey` / `llm.tiers.<tier>.apiKey`. The tenant-worker's pod-level env exposes them to `shell_exec`'s subprocess environment automatically.
4. **Multiple delegations race on the session directory.** Two `delegate_claude_code` calls in the same turn both operate in the same working directory — they conflict. Mitigation: spawn each as a subagent (subagent-subtree isolation, `components/session-filesystem.md`) — the primitive already exists. Prompt guidance should nudge the model toward subagent-spawn for parallel delegations.
5. **Cost accounting is real only for CLIs that emit it.** Claude Code emits `cost_usd`; Codex emits token counts (cost derived); Aider emits token counts (same); `gh`/`aws`/`docker` emit nothing (correctly — no LLM inside). Adapters that can't extract cost record `cost_usd = NULL`; downstream `budget-guardrails` sums `COALESCE(cost_usd, 0)`, which is honest (no cost data ≠ zero cost, but for deterministic CLIs there genuinely is none).
6. **Interactive/TTY-mode CLIs don't fit at all.** Every delegated CLI has a headless/print mode; this component only supports that mode. Interactive `gh auth login`, plain `claude-code` interactive REPL — not delegation shapes, deployment-time human operations.
7. **Delegations don't participate in the model registry's tier/hint machinery.** A `delegate_claude_code` call doesn't consult `LANGUAGE_MEDIUM_MODEL`; Claude Code has its own model config. Correctly — the model registry is for the orchestrator's own model calls, not the delegated agent's. Named honestly.
8. **The audit table can grow unbounded for a chatty CLI.** A 10-minute Claude Code call could easily emit hundreds of events. Not a design gap — the same growth story as `messages`/`tool_calls`, same eventual archival concern that applies to those tables applies here too. Bounded per tool call by the CLI's own event emission rate.
9. **Subagent-recursion + delegation compose oddly.** A subagent spawned by `spawn_subagent` could itself call `delegate_claude_code`, whose Claude Code process might itself internally decide to spawn subprocesses. The harness's own subagent lease isolation covers the harness side; what happens inside the delegated CLI is that CLI's own concern. Named honestly rather than hidden.

### Recommendations for the Implementation Pass (once approved)
- **Start with `delegate_claude_code` only.** One adapter, one CLI, real Claude Code invocations against a real tenant. Prove the shape end to end before adding a second adapter.
- **Land the `delegated_agent_events` migration first**, so audit is available from the very first `delegate_*` call. Don't retro-fit audit.
- **Wire cost into `budget-guardrails` metrics from day one**, even if it's just `cost_usd` per delegation logged as a counter. Named as "resolved" in that component's own dependency list — this closes the loop.
- **Write the adapter as a small Python module that reads stdout line-by-line**, `json.loads` each line, dispatches by `type` field to per-event handlers that write to `delegated_agent_events` and update the running summary struct. Same async-generator shape `openai_provider.py`'s streaming path already uses.
- **Version-pin Claude Code in the tenant-worker image** (`activities/Dockerfile` — pin to a specific released version, upgrade deliberately). Adapter's integration test uses that pinned version.
- **Do NOT add real-time signal-to-workflow** in this first pass. Postgres audit + structured summary are enough; signal streaming is a real, separate follow-up if a concrete use case appears.

### Open Questions / To Design
- **Prompt-level guidance: when should the model choose `delegate_claude_code` vs. hand-crafting `shell_exec` calls?** For nontrivial coding tasks the answer is "almost always delegate" (that's the whole vision); for one-line commands the answer is "just `shell_exec` — no need for a heavyweight delegation." A prompt-level nudge distinguishes; not designed here, belongs to the wider system-prompt restructure `docs/04-architecture-orchestrator-vision.md` calls out.
- **Per-delegation budget.** Should a `delegate_claude_code` call carry a `max_cost_usd` argument the harness enforces by killing the subprocess if the CLI's own cost accounting crosses it? Real safety valve, but also complicates the adapter (needs to consume cost events in real time, not just at exit). Deferred — first pass is just observability; enforcement can layer on top later.
- **Skill composition with delegation.** A very common shape will be "look up a skill via `get_skill`, then delegate to Claude Code with the skill content in the prompt." The model composes this manually today. Whether `delegate_claude_code` eventually gains a `skill_id` shortcut that fetches + embeds automatically is a real question, but deferred — build the raw pieces first, watch how they get composed.
- **Session ID / resume support.** Claude Code supports `--resume <session_id>` — an interrupted delegation could in principle be resumed rather than restarted from scratch. The harness would need to capture the session_id from the event stream, remember it (in `tool_calls.result` or a new column), and thread it into a follow-up `delegate_claude_code` call. Nontrivial state management (whose responsibility is "remember the interrupted session"?). Deferred until real interrupted-delegation use cases exist.
- **Aider-specific features** (multi-file coordination, git commits). Aider's event grammar is genuinely different from Claude Code's. Once a second adapter is warranted, this will be the first place its adapter-per-CLI design pays off — but naming it as future work rather than pretending it's already been designed.

### Notes Log
- 2026-08-28: **Scaffolded** as part of the orchestrator-vision documentation pass. Design is fully specified — adapter-per-CLI shape, `delegated_agent_events` table, structured-summary return, Tier B timing, real failure taxonomy, real cost/audit surface — but no code exists yet. Prompted directly by the user's direct question about firing the CLI with a custom stdout: turned out several major coding CLIs (Claude Code, Codex, Aider) already emit machine-readable event streams, which substantially closes the "opaque delegation" and "invisible cost" concerns raised in `docs/04-architecture-orchestrator-vision.md`. Adapter-per-CLI approach (rather than a generic "stream any tool"): each CLI's event grammar is genuinely different enough that a shared parser is either lowest-common-denominator or config-heavy — real thin Python adapters win, same reasoning `providers/*_provider.py` already uses. Every risk named plainly in the doc rather than left implicit; the two mitigable ones (adapter maintenance via version-pinning + integration tests, session-dir races via subagent-spawn) get concrete mitigations; the accepted ones (delegation is slow, credentials scale linearly, cost is only visible for CLIs that emit it) are called out honestly rather than glossed. Recommend starting implementation with `delegate_claude_code` only, single adapter end to end, before adding a second.
