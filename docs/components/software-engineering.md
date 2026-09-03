# Component: Software Engineering — Project Authoring, Build & Deploy

> STATUS: DESIGN (2026-09-02). Not built.
>
> Gives the agent a real software-engineering capability: it authors, builds,
> and deploys projects — automating the build/publish work the user does by
> hand today, and taking feature requests against real repos.
>
> **Decoupled from `proactivity.md` (2026-09-02).** An earlier draft made this
> a prerequisite for proactivity (the agent would build its own intention
> worker). That was rejected as redundant — proactivity is now first-class
> hand-built components. This capability stands on its own merits and has no
> dependents; build it when the user's SWE-automation need is the priority.
>
> This is the concrete realization of
> [`../04-architecture-orchestrator-vision.md`](../04-architecture-orchestrator-vision.md)
> for the coding lane: the harness stays the orchestrator, Claude Code does the
> code. The delegation mechanism already exists —
> [`delegated-agents.md`](delegated-agents.md) (`delegate_claude_code`, stream
> parsing, `delegated_agent_events` audit). This doc adds what makes it a
> *software-engineering* capability rather than a one-shot code call: a
> **persistent project workspace**, **Claude Code session continuity** across
> delegations, and a **build + deploy + verify** step.
>
> Builds on [`session-filesystem.md`](session-filesystem.md) (the tenant PV,
> leases), [`tool-registry.md`](tool-registry.md) (`shell_exec`),
> [`delegated-agents.md`](delegated-agents.md). Consumed by
> [`proactivity.md`](proactivity.md).

### Role

The agent can take a goal — "implement this feature in my API repo", "cut a
release", "add the metric the dashboard is missing" — and carry it through
authoring, building an image, and deploying to the cluster, ending with a
verified rollout or a clean rollback. It is the tech lead; Claude Code (the
CLI) is the implementer.

This replaces nothing — it's additive. Without it the agent still reasons and
runs shell commands per turn; with it, that work accumulates in a persistent
project it iterates on across sessions and ships.

### The decisions

#### Projects are filesystem, not a table

`/projects/<name>/` on the tenant PV (the same volume mounted at `/sessions`
today — the mount is widened, or a sibling path added). Each project is a git
repo, persistent across sessions and pod restarts.

- **Discovery** is `shell_exec` under `/projects` — `ls`, `find`, `git -C`,
  `cat PROJECT.md`. There is **no `projects` table**: a registry table would be
  duplicated state that drifts from the filesystem, and everything a table
  would hold is already discoverable from the repo. The filesystem *is* the
  registry.
- **Concurrency**: the existing `session_filesystem_leases` table is extended
  to cover `/projects/<name>/` — a turn working on a project takes a
  project-scoped lease, so two concurrent turns can't clobber each other.
  Single-tenant, so contention is rare; the lease is cheap insurance and reuses
  machinery that already exists.
- **Git remote**: projects may push to a real remote (the agent's own Gitea /
  a GitHub repo) for backup and so the user can read the code, but local-only
  on the PV is a valid state — the remote is not load-bearing. (Open question:
  default to a remote or not.)

#### Each project self-describes in `PROJECT.md`

A convention file at the repo root, the **operator's** view of the project
(distinct from the repo's `CLAUDE.md` / `AGENTS.md`, which is the
**implementer's** view — Claude Code reads that natively). `PROJECT.md`
covers:

- what the project is and does;
- **build** — the `make` targets that build and push the image, including the
  in-cluster variant (see "In-cluster builds" below);
- **deploy** — target namespace, Helm release name, values file, the exact
  `helm` invocation;
- **verify** — the health check / smoke test / expected `kubectl rollout`
  outcome that decides deploy success;
- operational gotchas.

Ground truth lives here, versioned with the code. The skill store's role is
*not* to duplicate this per project — it's to hold the **general** SWE
procedure ("discover under /projects → read PROJECT.md → `code_task` for code
changes → build → deploy → verify") and, after a successful deploy, the
**learned operational specifics** the RL loop records (the flag that mattered,
the step that's flaky, the wait that's too short). In-repo instructions +
earned operational memory on top.

#### `code_task` — `delegate_claude_code`, project-bound

`code_task(project, instruction, reset=false)` is
`delegate_claude_code` (`delegated-agents.md` — stream-json parsing, per-event
audit to `delegated_agent_events`, structured summary to the model, not the raw
stream, same cancellation contract as `shell_exec`) with two additions:

- **Project-bound working directory** — it runs in `/projects/<project>/`, not
  the ephemeral session dir. Same lease semantics, extended path.
- **Claude Code session continuity** — the CC session id is persisted in
  `/projects/<project>/.agent/cc-session` (a file). Each `code_task` runs
  `claude -p "<instruction>" --resume <session-id> --output-format stream-json`,
  so CC keeps its own context of the project across the agent's iterations.
  `reset=true` starts a fresh CC session (a new, unrelated goal).

The agent drives it **task-by-task**: scoped instruction → review the returned
diff + summary, run the tests via `shell_exec` → next instruction or
corrections. The agent decides when the project is ready to ship. Tier B
(heartbeat, ~20 min) — a coding task routinely runs 5–20 minutes.

Auth: a real Anthropic / subscription credential for the CLI, distinct from the
model-tier keys (`model-registry.md`).

#### Build and deploy are `shell_exec` + the project's own tooling

No bespoke pipeline. The agent runs `make <build>`, `make <push>`,
`helm upgrade ...` as `PROJECT.md` describes.

**Toolchain in the tenant-worker image** (per the decision to keep SWE work in
the tenant-worker, not a separate worker): `claude`, `helm`, `kubectl`, `git`,
`make`, and a **daemonless image builder** (`buildah` or `kaniko`).

**In-cluster builds** — a pod has no Docker daemon, so a `make build` target
that shells out to `docker build` won't run as-is. Each project's Makefile
carries an in-cluster variant (`make image` → buildah/kaniko), and `PROJECT.md`
documents it. This is a known rough edge — expected to need iteration per the
"let it fail, fix it constructively" stance.

**Registry**: a scoped robot credential (a `~/.docker/config.json` in the
tenant-worker), the agent's own repo path.

#### Deploy permissions — scoped, one hard boundary

The tenant-worker's Kubernetes service account gets `helm` / `kubectl` rights
in the namespace(s) the agent deploys to (a project declares its target in
`PROJECT.md`).

- **Hard boundary, not a gate**: the agent can **never** modify the harness's
  own workloads — `loop-worker` (`harness`), `tenant-worker`
  (`abishekk-worker`), `gateway`, and anything in the `core` namespace.
  Enforced twice: RBAC that simply can't, **and** a name-denylist check in the
  `deploy` activity before it runs `helm` / `kubectl`.
- **Approval gate**: the **first** deploy of any project (the agent proposes
  the full `helm` command + target; the user approves once), and any deploy
  targeting a namespace outside a known-safe set.
- Everything else: roughly the access the user has by hand today — single
  tenant, own cluster.

#### The dev + deploy loop rides the existing pipeline

Project work is a **Deliberate task** (`lane-model.md`) — no new workflow type:

- Its **episode** (`episode-lifecycle.md`) is the project-work session, spanning
  the turns it takes.
- Its **plan ledger** (`08-planning.md`) is the task breakdown ("scaffold the
  workflow", "add the activity", "write the test", "build", "deploy", "verify").
- `code_task`, `shell_exec`, and `deploy` are tools in the reason-act loop.
- Recording it feeds the skill loop — the general SWE procedure and the
  per-project operational specifics get learned.

The one piece with real orchestration is **`deploy`**, a Tier B activity that
does the sequence atomically: run the `make`/`helm` command → `kubectl rollout
status` (wait) → run the project's verify step → **on failure, `helm rollback`
/ revert and report failure**. The rollback is the *defined failure semantics*
of `deploy`, not a fallback path.

### Data model

**None new.**

| thing | lives in |
|---|---|
| project code & metadata | `/projects/<name>/` (git repo, `PROJECT.md`) |
| Claude Code session id | `/projects/<name>/.agent/cc-session` (file) |
| project locking | `session_filesystem_leases` (existing table, path extended) |
| delegation audit / cost | `delegated_agent_events` (`delegated-agents.md`, existing) |
| learned operational specifics | `skill_procedures` (existing) |

### Temporal shape

| unit | where | tier | does |
|---|---|---|---|
| `code_task` (`delegate_claude_code`, project-bound) | tenant-worker activity | B (heartbeat, ~20min) | `claude -p --resume` in the project dir → diff + summary; events → `delegated_agent_events` |
| `deploy` | tenant-worker activity | B (~10min) | `make`/`helm` → `rollout status` → verify → rollback on fail |
| project work overall | a normal Deliberate `TurnWorkflow` / episode | — | plan ledger = task breakdown; the above are tools |

### Degradation

Per the no-fallback principle. A build fails → the turn surfaces it, the agent
reports it, the cause gets fixed. A deploy fails verification → automatic
rollback (defined semantics) → surfaced. No "try another registry", no "deploy
anyway", no silent retry-forever.

### Deferred

- **Agent-authored sandboxed activities** — arbitrary Python beyond `code_task`.
  Until a real need forces it (`proactivity.md` explicitly bets the
  bounded set is enough).
- **A dedicated `swe-worker`** — folded into the tenant-worker for now. Revisit
  if the toolchain bloat, credential surface, or build blast-radius becomes a
  problem in practice.
- **Multi-tenant isolation** — namespaces per tenant, resource quotas, cost
  guardrails. Single-tenant now.
- **A real CI gate** — the agent runs tests via `shell_exec`; a proper
  pre-deploy CI pipeline is later.
- **In-cluster build ergonomics** — the buildah/kaniko path will iterate.

### Open Questions

- **`PROJECT.md` structure** — freeform prose, or a light structured header
  (YAML frontmatter) that the `deploy` activity parses for
  namespace/release/values so it isn't re-derived from prose each time?
- **`code_task` review depth** — does the returned summary + diff give the
  agent enough to review, or does it need a `code_show(project, path)` companion
  for "show me file X as it stands now"?
- **Git remote default** — push every project to a remote (backup, user
  visibility) or local-PV-only until the agent decides otherwise?
- **CC session lifetime** — one long CC session per project forever, or reset
  per goal? (A stale 200-turn CC session may drift; per-goal reset is cleaner
  but loses project familiarity.)
