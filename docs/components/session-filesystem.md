# Component: Session Filesystem
## (Shared Mount + Postgres-Backed Leases)

> STATUS: SCAFFOLD — to be filled in as we design.

### Role (one line)
Gives file-touching tool calls (shell exec, file edit, build tools, and anything git-like) a durable, worker-agnostic working directory per session — a real POSIX filesystem on a shared mount, with Postgres as the coordination layer that keeps concurrent writers from colliding. Exists so that workers stay stateless and interchangeable even for tools that need real files on disk, without pinning a session or subagent to a specific worker.

### Why this exists (recap)
Filesystem-touching tools break the "any worker can pick up any activity" statelessness the rest of this design relies on — a checkout or working directory has to survive across multiple tool calls within a session/subagent, and possibly across turns. Two options were considered and rejected before landing here:
- **Git worktrees pinned to one worker** — rejected. Requires per-subagent sticky task-queue routing (Temporal doesn't provide worker-pinning for activities out of the box), and a worker crash silently loses any uncommitted edits with no way for Temporal to recover them elsewhere — a durability regression against the whole point of this redesign.
- **Pure document store (no real filesystem)** — rejected. Most tools in a coding-agent harness (shell exec, build tools, language servers, git) expect a real POSIX filesystem; making that work against a document-store API would require building a filesystem-translation layer, which isn't worth the lift just to get atomic per-document writes.

**Resolved instead:** a real shared mount holds the bytes and the actual directory hierarchy (workers stay interchangeable — any worker can reach any session's files), and Postgres — already the durable store of record everywhere else in this design — holds per-resource lease/metadata rows. Same durable-store-plus-disposable-execution pattern used for conversation state, just applied to files.

### Responsibilities (from architecture)
- Provide a session-scoped working directory, mounted identically and simultaneously by every worker: `/session/{session_key}/`.
- Provide **per-subagent subdirectories**, isolated by default: `/session/{session_key}/sub/{n}/`, nested further for deeper recursion (mirrors the workflow-ID tree scheme already used for turns/subagents — see `components/temporal-workflow.md`).
- Arbitrate concurrent writers via **Postgres-backed leases** (not filesystem-level locking — NFS-style mounts don't give reliable atomic locking across clients).
- Support explicit **merge-back** of a completed subagent's file changes into its parent's working directory, driven by the subagent's result/observation rather than happening automatically or invisibly.

### Key Design Decisions (recap)
- **Hybrid storage, not a pure document store.** Real shared mount for bytes + hierarchy; Postgres for metadata and leases. One row per resource: session/path, content hash, last-writer, lease state.
- **Leases, not fixed-expiry locks.** A lock with a flat TTL would get stolen out from under a legitimately slow write (same failure mode we already rejected for activity cancellation via `ABANDON`). Instead: the holder renews the lease periodically while genuinely still working; it only expires if renewal actually stops (crash), giving deadlock-safety without punishing slow-but-alive writers.
- **Lease granularity starts at session/subagent-directory level**, with per-file leases as a plausible refinement once real contention patterns are observed — not committed to per-file today.
- **Per-subagent subdirectories, isolated by default.** A subagent's changes are not automatically visible to its parent or siblings; they're pulled in explicitly on completion via merge-back.
- **No worker pinning required.** Because the mount is shared and coordination lives in Postgres, any worker can service any tool-call activity for any session/subagent at any point — this preserves the stateless-worker-pool property everything else in this design depends on.

### Resolved: Subagent Merge-Back Mechanics
Reuses the existing lease schema entirely — no new table, no new snapshot/manifest mechanism needed.

- **Changed-file manifest** = a query over `session_filesystem_leases` for every row whose path falls under the subagent's own subtree (`/session/{key}/sub/{n}/...`). Since any file-touching tool call already acquires a lease, "what got leased under my subdirectory" already *is* the changed-file list — nothing new has to be tracked to produce it.
- **Merge stays explicit and model-driven**, consistent with "not automatically visible" above. The subagent's completion result (delivered to the parent as a tool-call observation, same as any subagent result) includes this manifest. The model, in its next reasoning step, decides whether to invoke a `merge_subagent_output` tool — merging all listed files or a chosen subset.
- **Merge activity and conflict detection reuse timestamps already in the lease table** — no new base-hash or snapshot mechanism required. For each file being merged: acquire a lease on the destination path in the parent's directory, then compare the destination's current `session_filesystem_leases.expires_at`-adjacent write timestamp (i.e. its last-write time) against the subagent child workflow's start time.
  - Destination untouched since the subagent started (or doesn't exist yet) → safe to copy over.
  - Destination written *after* the subagent's start time → something else (the parent itself, or a sibling subagent) wrote there concurrently. This is a real conflict — the file is **skipped, not silently overwritten**, and reported back to the model as part of the merge tool-call's result. Same pattern already used for cancellation's `side_effect: "unknown"`: don't silently resolve an ambiguous case, surface it honestly.

### Open Questions / To Design
- Lease granularity — session-directory-level vs. per-file — once real tool contention patterns exist to inform the choice. (Merge-back's conflict check above works at whatever granularity leases end up at; not blocked on this being resolved first.)
- Concrete shared-mount technology (NFS/EFS-equivalent vs. clustered filesystem vs. cloud-specific) — deliberately left open for implementation time, same as the outbound-queue broker choice in `components/activities-outbound-delivery.md`. Now also needs to support hosting a Postgres data directory adequately (fsync/durability behavior matters more for Postgres than for the session tree) — a consideration this choice didn't originally have to account for.
- Lease table schema and exact columns (session/path, holder identity, lease expiry, renewal interval) — belongs in `components/state-layer.md`'s schema design.
- Interaction with session consolidation/archival (`future-work.md` — session management/consolidation): if a session's raw files are ever archived, the turn workflow's context-hydration step needs to notice and transparently rehydrate before a tool call proceeds.
- Exact large-vs-small payload threshold for the Postgres-vs-PV claim-check split (what counts as "large enough to route to the PV instead of Postgres") — not yet numerically specified.
- PV sizing/IO capacity planning given it now serves two workloads (Postgres + session files) — flagged as a cost in `components/multi-tenancy.md`, not sized here.

### Resolved: Multi-Tenant Access Scoping
Originally flagged as an open caveat: a shared mount means every worker's tool-execution process can technically reach every session's directory unless scoped by OS/container-level permissions. Resolved via `components/multi-tenancy.md` — **one dedicated PersistentVolume per tenant namespace**, mounted only by that tenant's own dedicated worker fleet. The mount itself is still shared *within* one tenant (across that tenant's own sessions/subagents, coordinated by the leases above) — nothing here changes. What changes is the boundary: "every worker" now means "every worker in this tenant's fleet," not every worker globally, because tenants no longer share a worker pool at all. No filesystem-specific mechanism was needed beyond that — the tenant boundary is the PV boundary.

(This applies to the **activity-worker** fleet specifically — the tenant boundary described here is unaffected by the shared-workflow-worker-pool decision in `components/multi-tenancy.md`, since workflow workers never touch this mount at all under the reference-passing contract in `components/temporal-workflow.md`.)

### Resolved: The Tenant PV Also Hosts That Tenant's Postgres — and Serves as the Claim-Check Store for Large Content
**Introduced 2026-08-14**, from two related decisions in `components/multi-tenancy.md`:

1. **Same PV, disjoint subpaths.** A tenant's Postgres instance (self-hosted, one per tenant — see `components/multi-tenancy.md`, revised from "separate database" to "separate instance") uses this same PV for its data directory, under a path disjoint from the session tree — e.g. `/data/postgres/` (Postgres's `PGDATA`) alongside `/data/sessions/` (this component's existing tree). One volume, two workloads, no file-level contention between them — only shared disk IO capacity, a capacity-planning cost named in `components/multi-tenancy.md`, not a correctness one.
2. **This PV is also the "claim check" store for large content the workflow shouldn't hold.** Per `components/temporal-workflow.md`'s reference-passing contract (introduced the same day, for the same multi-tenancy motivation): Postgres holds structured/modest-sized content (messages, tool-call metadata); large payloads — big tool outputs, file diffs — are written here instead and referenced by path/lease, using the **same lease mechanism already designed above**, not a new one. A large tool result becomes just another leased file under the session tree; nothing new to build.

**Why one Postgres instance on this PV doesn't reopen the "shared mutable file on a shared mount" hazard this component already rejected once** (git-worktree-per-worker, then implicitly SQLite-per-tenant-PV when that was considered and rejected in `components/multi-tenancy.md`): there is exactly **one** Postgres process per tenant, and it owns its data directory exclusively — the same single-writer-per-file-tree property this component's own leases already enforce for the session filesystem, just achieved by Postgres's own startup lock rather than by an explicit lease row. The multi-replica activity worker fleet talks to that one Postgres instance over the network like any Postgres client — it never touches `/data/postgres/` directly, so the fleet being multi-replica doesn't reintroduce a multi-writer-to-the-same-files problem.

### Notes Log
- 2026-08-07: Introduced this component to resolve subagent filesystem isolation. Rejected git-worktree-per-worker (durability regression, requires worker pinning) and pure document store (breaks POSIX-expecting tools) in favor of a shared mount + Postgres-backed lease index. Resolved: per-subagent subdirectories (not a shared workspace), lease-with-renewal (not fixed-expiry) locking.
- 2026-08-07: Resolved the multi-tenant access-scoping caveat via `components/multi-tenancy.md`'s namespace-per-tenant decision — dedicated PV per tenant, mounted only by that tenant's dedicated worker fleet. No change to the intra-tenant design (leases, subagent subdirectories) — the isolation boundary just moved from "doesn't exist" to "the tenant's own worker fleet."
- 2026-08-14: Resolved subagent merge-back mechanics. Changed-file manifest is a query over existing `session_filesystem_leases` rows under the subagent's subtree (no new tracking needed); merge stays explicit via a model-invoked `merge_subagent_output` tool; conflict detection reuses lease write timestamps against the subagent's start time, skipping (not overwriting) and reporting any file touched concurrently by something else.
- 2026-08-14: This PV now also hosts the tenant's self-hosted Postgres data directory (disjoint subpath from the session tree) and serves as the claim-check store for large content under `components/temporal-workflow.md`'s reference-passing contract — both stemming from the same-day multi-tenancy revision in `components/multi-tenancy.md`. Confirmed one Postgres instance per tenant doesn't reopen the multi-writer-on-shared-mount hazard this component already designed around, since Postgres's own exclusive-data-directory lock plays the same role its own leases play for the session tree.
