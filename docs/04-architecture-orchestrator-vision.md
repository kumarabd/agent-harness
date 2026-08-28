# Agent Harness — Distributed, Scalable Architecture
## Part 4: The Orchestrator Vision — Leverage, Don't Rebuild

This doc records a genuinely load-bearing shift in what this harness *is*, articulated 2026-08-28. Parts 1–3 described the harness in the shape of a peer to Claude Code / Codex / OpenClaw — a competitor doing its own agent loop, its own file tools, its own web fetches. Part 4 makes explicit that the harness is instead **an orchestrator on top of** those excellent single-purpose tools, delegating load-bearing work to whichever specialist CLI is best at it, retaining the orchestration itself as this project's actual contribution.

---

### The Shift, Plainly

**Old framing** (implicit through Parts 1–3):
- The harness runs its own reason–act–observe loop over its own tools.
- For coding: build first-class `read_file`, `edit_file`, `write_file`, `grep`, `glob`.
- For web: build first-class `fetch` / `browser`.
- Compete with Claude Code / Codex / Aider on quality of file semantics, cost management, tool ergonomics.

**New framing** (this doc):
- The harness runs its own reason–act–observe loop over a much *narrower* set of tools.
- For coding: delegate to Claude Code / Codex / Aider via a specialized `delegate_agent`-style tool (`components/delegated-agents.md`) that spawns the CLI with structured event streaming, parses events for real audit + cost, returns a clean summary.
- For web: delegate to whichever CLI does that best (or route via mcp-hub if there's already a good backend).
- For skills / procedural guidance: leverage mcp-hub's existing `search_skills`/`get_skill` (`components/skills.md`) — the same shape as `search_tools`/`call_tool` already resolved for the mcp-hub tier.
- **The harness's contribution is the orchestration itself**: multi-turn, multi-tenant, durable, cancellable, subagent-recursive, cost-tracked. Not another Claude Code.

---

### Why This Is Coherent (Not a Cop-Out)

**1. Temporal is *good* at orchestration.** Long waits, retries, cancellation, durability, replay — exactly what a delegated-CLI world produces (30-second to 10-minute inner calls, real failures partway, real cost, real audit trails). Building yet another set of native file tools plays *against* Temporal's strengths; orchestrating specialist CLIs plays *directly to* them.

**2. There are 5+ excellent coding CLIs already.** Claude Code, Codex, Aider, Cursor CLI, Gemini CLI — each backed by real teams, real integrations, real cost management, real prompt engineering. Reproducing any single one is a huge undertaking; picking the right one per task and running it durably is a genuinely different, smaller, more useful thing.

**3. mcp-hub already gives us discovery mechanics, for both tools and skills.** `search_tools`/`call_tool` for capabilities that live behind APIs (GitHub, Notion, etc.); `search_skills`/`get_skill` for procedural knowledge. The harness's own tool surface (`shell_exec`, memory_*, model registry, subagents) plus these two mcp-hub-mediated tiers plus the new `delegate_*` tier cover an enormous range of tasks without the harness ever needing to build a "better `edit_file`."

**4. The dominant coding CLIs emit structured event streams.** Claude Code's `--output-format stream-json`, Codex's `codex exec --json`, gh's `--json`, Aider's `--stream` — real, documented, machine-readable. This means "delegate" doesn't mean "black box" — the orchestrator can genuinely see the tool calls the delegated agent made, the files it touched, the cost it incurred. See `components/delegated-agents.md` for the plumbing.

---

### What This Changes vs. What Stays

**Stays exactly as-is (Parts 1–3 remain accurate):**
- Session Coordinator + Turn Workflow split.
- Reference-passing contract.
- Multi-tenancy design (namespace-per-tenant + per-tenant workers/Postgres/PV).
- Model registry, per-tier providers, Anthropic + OpenAI-compatible support.
- Context slot (LCM), memory slot (agent-brain), user-input (approval-gating).
- Session filesystem + claim-check large-payload routing.
- Existing tools: `shell_exec`, `memory_search`/`memory_expand`, `search_tools`/`call_tool`, `merge_subagent_output`, `declare_next_step_hint`.
- Subagent recursion (still the right primitive for parallel independent work).

**Changes in emphasis:**
- **Model registry usage skews smaller.** If Claude Code (or Codex) is doing the load-bearing model calls, the orchestrator's own model tier is mostly `fast` — routing decisions, "which delegated tool," summarization. `expert` tier is rarely used by the orchestrator itself.
- **Cost accounting relocates.** Real cost is now largely inside delegated calls, not the orchestrator's own model calls. `budget-guardrails.md` needs to consume `delegated_agent_events.cost_usd` (see below), not just orchestrator token counts.
- **System prompt reshapes around orchestration, not execution.** The current prompt says "you're a coding assistant, use `shell_exec`." Under this vision it should say "you're an orchestrator; here's how to decide between `delegate_claude_code`, `search_tools`/`call_tool`, `search_skills`, `shell_exec`, and spawning a subagent." Not designed here — flagged as a follow-up in `components/skills.md` and `components/delegated-agents.md`.

**Drops off the roadmap entirely:**
- Native `read_file`, `edit_file`, `write_file`, `grep`, `glob` — deliberately not built. Claude Code / Codex / Aider are better at these than any native tool this project could reasonably ship.
- Native `fetch` / `browser` — same reasoning; delegate or route via mcp-hub.
- Elaborate per-command approval taxonomy — the current flat `PERMISSION_LIST` covers the `{server,tool}` axis; approval within a delegated agent's execution is that CLI's own concern, not this harness's.

---

### The Genuinely Real Risks (Named, Not Glossed Over)

**1. Opaque delegation, unless you use structured event streams.** A plain `shell_exec claude-code "..."` returns only stdout — you don't see the inner tool calls, files touched, or cost. **Mitigation**: the `delegate_agent` shape in `components/delegated-agents.md` is designed around each CLI's own structured-event format (Claude Code's `stream-json`, Codex's `--json`, etc.), which reduces this risk substantially. Adapters extract tool calls, cost, files touched into a Postgres audit table.

**2. Invisible cost — dissolved for CLIs that emit it, real for those that don't.** Claude Code and Codex both emit token counts and cost in their event streams; adapters capture that. `gh`, `aws`, `docker` have no LLM inside — no cost to capture, correctly. So cost visibility is real for exactly the calls where it matters.

**3. Delegated tools are slow.** Real Claude Code invocations are often 30 seconds to 10+ minutes. Today `shell_exec` is Tier B (5-minute `StartToCloseTimeout`); `delegate_*` needs a longer default. Not a design change; a timing config change.

**4. Auth / credential explosion.** Each delegated CLI wants its own credentials — `ANTHROPIC_API_KEY` for Claude Code, `OPENAI_API_KEY` for Codex, `GH_TOKEN` for gh, `AWS_*` for aws. **Accepted, per direct user direction 2026-08-28**: "credentials for the CLI is fine, there are ways to handle them." The onboarding cost scales linearly with adoption, but each addition is a discrete secret in the tenant's own Helm overrides (`deploy/helm/tenants/<tenant>.yaml`), following the same convention `agentBrain.apiKey` / `llm.tiers.<tier>.apiKey` already use.

**5. Multi-turn interactive CLIs don't fit `shell_exec`'s shape at all.** `shell_exec` is one-shot: send a command, wait for exit, get stdout. Works fine for the "headless print mode" every major coding CLI supports (`-p`, `--print`, `exec`). Doesn't work for interactive TTY sessions (`gh auth login`, plain-mode `claude-code`). **Not a gap in this vision** — every load-bearing CLI has a headless mode. Interactive commands aren't a delegation shape; they're operational commands the human runs at deploy time.

**6. Delegations race on the session directory.** Two `delegate_claude_code` calls fired in the same turn both operate in the same session working directory — they'll conflict. **Mitigation**: fire each as a subagent (subagent subtree isolation is already built, `components/session-filesystem.md`), or serialize the calls. The existing subagent primitive covers this cleanly.

**7. Adapter maintenance burden.** Each supported CLI is an adapter. Claude Code's `stream-json` format changes → adapter breaks. Same shape as mcp-hub's manifest-versioning open question. **Mitigation** (in `components/delegated-agents.md`): version-pin the delegated CLI in the tenant-worker image, keep adapters thin (parse a handful of fields, don't model the whole event grammar), fail loud on unknown event types, test each adapter with a real CLI invocation as part of image build.

**8. Model registry loses meaning if delegation dominates.** If 90% of real model work happens inside delegated CLIs, the harness's own tier/escalate/hint machinery is mostly bookkeeping over infrequent routing calls. **Accepted honestly** — the machinery is already built and lightweight enough that this isn't a real cost, just a mismatch between the sophistication of the mechanism and the load it actually carries.

---

### The Two Concrete Components This Vision Adds

- **`components/skills.md`** — `search_skills` and `get_skill` as two new native tools, structurally identical to the already-resolved `search_tools`/`call_tool` from `components/tool-registry.md`'s mcp-hub-mediated tier. Skills are guidance documents ("how to do X in this environment"), not executable recipes — the model reads a skill and *decides* what to invoke (`shell_exec`, `delegate_*`, `call_tool`, etc.) from that guidance. Deliberately not a new execution mechanism; a new discovery mechanism.
- **`components/delegated-agents.md`** — the `delegate_*` tool family, one adapter per supported CLI. Each adapter knows its CLI's structured-event format, spawns the CLI, streams+parses events into Postgres for audit + cost accounting, returns a clean summary to the orchestrator. Substantially closes the "opaque delegation" and "invisible cost" concerns above.

Both are new; both fit the existing patterns (mcp-hub-mediated native tools, session-filesystem-scoped execution, Postgres-persisted per-tool metadata, reference-passing contract).

---

### Notes Log
- 2026-08-28: **Introduced.** Prompted directly by the user's articulation of the vision — "for coding tasks, I could use claude-code cli for eg. in my shell, with shell_exec, I would want to orchestrate the tasks in it. For skills, I would want to leverage search_skill and get_skill tools, similar to search_tool and call_tool. The idea is to offload the load-bearing parts out while still retaining immense quality." That's a coherent-enough shift in what this harness is that it deserves its own top-level architectural doc, not just a component-level note buried inside `tool-registry.md`. All Part 1–3 design work stays valid; this doc reframes what to build *next* rather than what already exists. Two new component docs (`components/skills.md`, `components/delegated-agents.md`) landed alongside this one, capturing the concrete mechanisms.
