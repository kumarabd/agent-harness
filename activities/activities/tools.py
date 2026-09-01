"""Tool registry and real tool implementations for the ToolCall activity
(tool_call.py). Each entry declares its heartbeat tier per
docs/components/activities-outbound-delivery.md's "Resolved: Heartbeat
Interval Policy" — Tier A (fire-and-complete, no heartbeat) isn't
represented here since every tool in this registry is real, non-trivial
work; only Tier B ("long-running, timer-chunkable") is implemented so far.

`shell_exec` is deliberately `non_idempotent` with no per-command safety
classifier — a smarter per-invocation idempotency classifier was explicitly
considered and rejected as over-engineering (docs/future-work.md §4's
"shell_exec classifier" note): idiomatic Temporal practice for a
non-provably-idempotent activity is a hard MaximumAttempts cap, not
inference at retry time. That cap lives on the Go side's ActivityOptions
(workflows/internal/workflow/turn.go), not here.

Also registers `search`/`slow_tool`/`noop_tool` against `_demo_echo_stub` —
these are workflows/scenarios/'s pre-existing fixture tool names (predating
this module), never real tools, kept working exactly as before (same
simulated delay, same cancellability) purely for backward compatibility with
that already-verified scenario suite. This is NOT a fallback for arbitrary
unregistered names — tool_call.py's "unknown tool" error path is real and
applies to anything not explicitly listed here.

`memory_search`/`memory_expand` (docs/components/memory-slot.md, "Resolved:
Search/Expand Tools — Both Unrestricted") are named to match agent-brain's
own MCP tool names directly, NOT memory-slot.md's proposed generic
`search`/`expand` — that generic `search` name is already taken by the
fixture stub above (predates this feature, and workflows/scenarios/ depends
on it staying a stub). No tier of its own: both are quick request/response
network calls with no cancellable subprocess or session-filesystem lease to
hold, so they fall back to tool_tiers.go's defaultToolTiming on the Go side
(no entry needed there) and don't call activity.heartbeat() here.

`search_tools`/`call_tool` (docs/components/tool-registry.md, "Resolved:
mcp-hub-Mediated Integration Mechanism") are the mcp-hub-mediated tier's own
tools, named to match mcp-hub's real MCP tool names directly, same reasoning
as memory_search/memory_expand above — same no-tier-of-its-own treatment.
search_tools additionally fans out to shell_hub.py's local, in-process
discovery ("Resolved: Native-Tool Discovery") and returns the combined
result.
"""

from __future__ import annotations

import asyncio
import os
import shutil
import signal
from dataclasses import dataclass
from typing import Any, Awaitable, Callable

import asyncpg
from temporalio import activity
from temporalio.exceptions import CancelledError

from . import agent_brain, claim_check, ids, lcm, leases, mcp_hub, shell_hub

_SESSION_ROOT_ENV = "SESSION_ROOT"
# Local-dev fallback; real deployments set SESSION_ROOT to match the Helm
# chart's PV mount (deploy/helm/agent-harness-tenant's tenantWorker mounts
# the session filesystem tree at /sessions).
_DEFAULT_SESSION_ROOT = "/tmp/agent-harness-sessions"

# Per-stream (stdout/stderr independently) large-output policy — the
# threshold and behavior both live in claim_check.py now
# (docs/components/session-filesystem.md, "Resolved: This PV Serves as
# the Claim-Check Store for Large Content"): outputs above the threshold
# get written to the PV under the tool's own session directory and
# returned as a reference the model can `cat`/`head`/`tail`/`grep` via
# ordinary shell_exec, instead of being flat-truncated and silently
# dropped as the pre-claim-check code did.


def resolve_session_dir(fs_path: str) -> str:
    """Resolves a session_fs_path (e.g. "/session/sess-1/sub/1/", from
    ids.session_fs_path) against SESSION_ROOT into a real filesystem path -
    env var read at the point of use, same convention as db.py's POSTGRES_*
    handling, no shared config module."""
    root = os.environ.get(_SESSION_ROOT_ENV, _DEFAULT_SESSION_ROOT)
    return os.path.join(root, fs_path.lstrip("/"))


@dataclass
class ToolContext:
    """Everything a tool handler needs beyond its own arguments: DB access
    for lease renewal, the resolved working directory, and this call's
    unique lease-holder identity.

    tool_call_id is threaded through so handlers producing artifacts on
    disk (claim_check.py's large-output route, currently) can name those
    artifacts deterministically per-call rather than racing on a shared
    name — one tool call owns exactly one claim-check filename slot.

    summary_provider is a Provider instance (providers/base.py) resolved
    from the medium tier's config — needed here so the
    exploration_summary.py LLM path (unstructured text tier) has
    somewhere to send its natural-language summary request. `None` when
    the tier isn't fully configured (missing PROVIDER/MODEL/API_KEY/
    BASE_URL); every consumer of this field tolerates that (degrades to
    a deterministic-only summary)."""

    pool: asyncpg.Pool
    session_key: str
    fs_path: str
    session_dir: str
    holder_id: str
    tool_call_id: str
    summary_provider: Any
    summary_model: str
    heartbeat_interval_seconds: float
    lease_ttl_seconds: float


@dataclass
class ToolSpec:
    handler: Callable[[dict, "ToolContext"], Awaitable[dict]]
    heartbeat_interval_seconds: float
    heartbeat_timeout_seconds: float
    start_to_close_timeout_seconds: float


async def _acquire_lease_blocking(ctx: ToolContext) -> None:
    """Waits for this call's session-directory lease, heartbeating and
    checking for cancellation between attempts rather than relying on
    Temporal's outer RetryPolicy - that has its own backoff semantics
    unrelated to this tool's heartbeat tier, and wouldn't stay
    heartbeating/cancellable while queued behind another holder."""
    while True:
        acquired = await leases.acquire_or_renew(
            ctx.pool, ctx.session_key, ctx.fs_path, ctx.holder_id, ctx.lease_ttl_seconds
        )
        if acquired:
            return
        activity.heartbeat()
        if activity.is_cancelled():
            raise CancelledError("tool call cancelled while waiting for session directory lease")
        await asyncio.sleep(ctx.heartbeat_interval_seconds)


async def _terminate_process_group(proc: "asyncio.subprocess.Process", grace_seconds: float = 2.0) -> None:
    """Actively tears down a running subprocess and its whole process group
    (not just the shell) - the cooperative-cancellation contract requires
    this, not merely stopping heartbeats and letting it dangle
    (docs/components/activities-outbound-delivery.md)."""
    try:
        os.killpg(proc.pid, signal.SIGTERM)
    except ProcessLookupError:
        return
    try:
        await asyncio.wait_for(proc.wait(), timeout=grace_seconds)
        return
    except asyncio.TimeoutError:
        pass
    try:
        os.killpg(proc.pid, signal.SIGKILL)
    except ProcessLookupError:
        return
    await proc.wait()


async def shell_exec(arguments: dict, ctx: ToolContext) -> dict:
    command = arguments.get("command")
    if not isinstance(command, str) or not command.strip():
        raise ValueError("shell_exec requires a non-empty string 'command' argument")

    os.makedirs(ctx.session_dir, exist_ok=True)

    await _acquire_lease_blocking(ctx)
    proc: asyncio.subprocess.Process | None = None
    try:
        try:
            proc = await asyncio.create_subprocess_shell(
                command,
                cwd=ctx.session_dir,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                # Own process group so cancellation's killpg reaches the
                # whole subtree the shell may have spawned, not just the
                # shell itself.
                start_new_session=True,
            )
            while True:
                try:
                    await asyncio.wait_for(proc.wait(), timeout=ctx.heartbeat_interval_seconds)
                    break  # exited naturally
                except asyncio.TimeoutError:
                    pass
                activity.heartbeat()
                await leases.acquire_or_renew(
                    ctx.pool, ctx.session_key, ctx.fs_path, ctx.holder_id, ctx.lease_ttl_seconds
                )
                if activity.is_cancelled():
                    raise CancelledError("tool call cancelled")
            stdout_bytes, stderr_bytes = await proc.communicate()
        except (asyncio.CancelledError, CancelledError):
            # Temporal delivers cancellation by cancelling the underlying
            # asyncio task directly - it can interrupt ANY currently-active
            # await (proc.wait() above, proc.communicate(), even a future
            # heartbeat/lease-renew call), not only the explicit
            # is_cancelled() check. So teardown has to happen here, in one
            # place that catches cancellation regardless of where it landed
            # - not only on the polling path. Shielded so a second
            # cancellation mid-teardown can't interrupt the kill itself
            # (found via a real test: without this restructuring, the
            # subprocess was silently leaked - status='cancelled' in
            # Postgres but the real process kept running).
            if proc is not None:
                await asyncio.shield(_terminate_process_group(proc))
            raise
    finally:
        await leases.release(ctx.pool, ctx.session_key, ctx.fs_path, ctx.holder_id)

    # Each stream is independently either inlined or routed through the
    # claim-check store — big stdout with tiny stderr (or vice versa)
    # shouldn't drag the small stream through the PV too. The returned
    # value under each key is either {"inline": text} (small) or a
    # reference dict with head/tail/exploration_summary/claim_check_path
    # (large); the model sees the same key regardless. See claim_check.py
    # and exploration_summary.py.
    stdout_result = await claim_check.store_if_large(
        ctx.session_dir, ctx.tool_call_id, "stdout", stdout_bytes,
        summary_provider=ctx.summary_provider, summary_model=ctx.summary_model,
    )
    stderr_result = await claim_check.store_if_large(
        ctx.session_dir, ctx.tool_call_id, "stderr", stderr_bytes,
        summary_provider=ctx.summary_provider, summary_model=ctx.summary_model,
    )
    return {
        "exit_code": proc.returncode,
        "stdout": stdout_result,
        "stderr": stderr_result,
    }


async def merge_subagent_output(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/session-filesystem.md, "Resolved: Subagent Merge-Back
    Mechanics" — model-invoked, explicit merge of a completed subagent's file
    changes into its parent's working directory.

    Args:
      subagent_turn_id: str — the subagent whose files to merge. Must be a
        direct child of THIS tool call's own turn (ctx.session_key +
        parent_id check below); merging from a random other subagent isn't
        supported and would break the parent-dir lease scoping.
      files: optional list[str] of relative paths to merge (a subset of the
        SubagentManifest's changed_files). Omitted / None / empty → merge
        every file in the subagent's subtree.

    Conflict rule (per the doc): for each source file, if the destination in
    the parent's directory has an mtime newer than the subagent's own
    `turns.started_at`, something else wrote there concurrently — skip and
    report, don't silently overwrite. Same "surface honestly, don't silently
    resolve" pattern the cancellation `side_effect: unknown` observation
    already uses.

    Also surfaces two edge cases the doc's rule doesn't explicitly call out:
      - destinations that already existed before the subagent started and
        were overwritten by the subagent's work (no conflict — this is the
        whole point — but named in the result so the model isn't surprised).
      - destinations the parent created concurrently (would-be conflict) —
        skipped, reported.
    """
    subagent_turn_id = arguments.get("subagent_turn_id")
    if not isinstance(subagent_turn_id, str) or not subagent_turn_id:
        raise ValueError("merge_subagent_output requires a string 'subagent_turn_id'")

    # Restrict merges to direct children of this tool call's own turn — the
    # parent-directory lease below is scoped to ctx.fs_path (this call's
    # own turn's dir), so accepting a random subagent_turn_id from
    # elsewhere in the session would compute a source path that doesn't
    # sit under the leased destination and could race against writers we
    # aren't coordinating with.
    expected_parent_prefix = ctx.fs_path.rstrip("/") + "/sub/"
    subagent_fs_path = ids.session_fs_path(subagent_turn_id)
    if not subagent_fs_path.startswith(expected_parent_prefix):
        raise ValueError(
            f"merge_subagent_output: {subagent_turn_id!r} is not a direct subagent "
            f"of this turn ({ctx.fs_path!r})"
        )

    row = await ctx.pool.fetchrow(
        "SELECT started_at FROM turns WHERE turn_id = $1",
        subagent_turn_id,
    )
    if row is None:
        raise ValueError(f"merge_subagent_output: no turns row for {subagent_turn_id!r}")
    subagent_started_at = row["started_at"].timestamp()

    source_root = resolve_session_dir(subagent_fs_path)
    dest_root = ctx.session_dir

    if not os.path.isdir(source_root):
        return {"merged": [], "skipped_conflicts": [], "overwrote_parent_earlier": []}

    requested_files = arguments.get("files")
    if requested_files is None:
        candidates: list[str] = []
        for dirpath, dirnames, filenames in os.walk(source_root):
            # Same pruning as subagent_manifest.py — a subagent's
            # tool-output claim-check artifacts aren't merge candidates,
            # they belong to that subagent's own turn's lifecycle.
            dirnames[:] = [d for d in dirnames if not claim_check.is_claim_check_dir(d)]
            for name in filenames:
                absolute = os.path.join(dirpath, name)
                if not os.path.isfile(absolute):
                    continue
                candidates.append(os.path.relpath(absolute, source_root))
        candidates.sort()
    else:
        if not isinstance(requested_files, list) or not all(isinstance(p, str) for p in requested_files):
            raise ValueError("merge_subagent_output: 'files' must be a list of strings if provided")
        candidates = list(requested_files)

    os.makedirs(dest_root, exist_ok=True)
    await _acquire_lease_blocking(ctx)
    merged: list[str] = []
    skipped_conflicts: list[dict] = []
    overwrote_parent_earlier: list[str] = []
    missing_sources: list[str] = []
    try:
        for rel in candidates:
            # Reject path escapes — a subagent-supplied name that resolves
            # outside the subtree could otherwise clobber unrelated
            # tenant files. commonpath handles both '..' and absolute paths.
            source = os.path.abspath(os.path.join(source_root, rel))
            dest = os.path.abspath(os.path.join(dest_root, rel))
            if os.path.commonpath([source_root, source]) != os.path.abspath(source_root):
                skipped_conflicts.append({"path": rel, "reason": "path_escapes_source"})
                continue
            if os.path.commonpath([dest_root, dest]) != os.path.abspath(dest_root):
                skipped_conflicts.append({"path": rel, "reason": "path_escapes_destination"})
                continue
            if not os.path.isfile(source):
                missing_sources.append(rel)
                continue

            dest_exists = os.path.exists(dest)
            if dest_exists:
                dest_mtime = os.stat(dest).st_mtime
                if dest_mtime > subagent_started_at:
                    skipped_conflicts.append(
                        {"path": rel, "reason": "destination_written_after_subagent_started"}
                    )
                    continue
                # Destination existed before the subagent started; the copy
                # below will overwrite the parent's earlier version. This
                # is exactly what merge-back is for, but surface it so the
                # model isn't surprised the parent's prior write is gone.
                overwrote_parent_earlier.append(rel)

            os.makedirs(os.path.dirname(dest), exist_ok=True)
            # copy2 preserves the source's mtime, which matters for a
            # follow-up merge from a sibling subagent: the just-copied
            # destination's mtime is now the subagent's write time, not
            # `now()`, so the conflict rule stays coherent across a chain
            # of merges.
            shutil.copy2(source, dest)
            merged.append(rel)

            # Cooperative-cancellation contract — a merge over many files can
            # take real time, so heartbeat and renew per file the same way
            # shell_exec heartbeats between its own timed waits.
            activity.heartbeat()
            await leases.acquire_or_renew(
                ctx.pool, ctx.session_key, ctx.fs_path, ctx.holder_id, ctx.lease_ttl_seconds
            )
            if activity.is_cancelled():
                raise CancelledError("merge_subagent_output cancelled")
    finally:
        await leases.release(ctx.pool, ctx.session_key, ctx.fs_path, ctx.holder_id)

    result: dict = {
        "merged": merged,
        "skipped_conflicts": skipped_conflicts,
        "overwrote_parent_earlier": overwrote_parent_earlier,
    }
    if missing_sources:
        result["missing_sources"] = missing_sources
    return result


async def memory_search(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/memory-slot.md's `search` — shallow, unrestricted
    (available to both the main agent and subagents; tool-level access
    control, if ever added, is components/tool-registry.md's concern, not
    this handler's). arguments passed straight through as agent-brain's own
    memory_search params (query, limit) — the model already speaks
    agent-brain's schema directly, no reshaping needed."""
    return await agent_brain.call_tool("memory_search", arguments)


async def memory_expand(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/memory-slot.md's `expand` — deep, also unrestricted
    (agent-brain's memory_expand has no depth parameter to escalate through,
    so the unbounded-re-expansion risk that would motivate a subagent-only
    restriction doesn't apply — see that doc's "Resolved: Search/Expand
    Tools"). arguments passed straight through (node_id, node_type, limit)."""
    return await agent_brain.call_tool("memory_expand", arguments)


async def discover_tools(query: str, top_k: int = 5) -> list[dict]:
    """The core of search_tools, ctx-free so both the model-facing tool
    handler below AND the request pipeline's ToolDiscover activity
    (docs/components/request-pipeline/07-tool-discovery.md) can call it.

    Fans the query out to mcp-hub (real, unchanged) and shell-hub (local,
    in-process) and returns one combined list of {server, tool, description,
    input_schema[, score]}. Gracefully returns only whichever source is
    actually configured/available rather than erroring
    (mcp_hub.McpHubNotConfiguredError mirrors agent_brain's own
    not-configured shape).

    top_k is the combined total, not a per-source budget — found via live
    testing (docs/components/tool-registry.md's Notes Log): passing the full
    top_k to both sources and concatenating meant a "top 5" request returned
    up to 10. Split evenly, mcp-hub (the curated primary tier) taking the
    remainder on an odd split — shell-hub is the supplementary local tier,
    not an equally-weighted peer corpus."""
    mcp_hub_top_k = top_k - top_k // 2
    shell_hub_top_k = top_k // 2

    try:
        raw = await mcp_hub.call_tool("search_tools", {"query": query, "top_k": mcp_hub_top_k})
    except mcp_hub.McpHubNotConfiguredError:
        raw = []
    # FastMCP (mcp-hub's own server framework) wraps a tool's non-object
    # return value in {"result": ...} — MCP's structured_content must be a
    # JSON object, and search_tools' real return is a bare list — confirmed
    # by a real call against the live cluster, not assumed from the spec
    # text alone.
    mcp_hub_results = raw["result"] if isinstance(raw, dict) and "result" in raw else raw
    shell_hub_results = await shell_hub.search(query, shell_hub_top_k)
    return list(mcp_hub_results) + shell_hub_results


async def search_tools(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/tool-registry.md, "Resolved: mcp-hub-Mediated
    Integration Mechanism" + "Resolved: Native-Tool Discovery" — the
    model-facing wrapper around discover_tools: one combined list so
    discovery is central from the model's perspective in one tool call. A
    deployment with no mcp-hub or no shell-hub catalog still works, just
    with fewer discoverable tools."""
    return {"results": await discover_tools(arguments.get("query", ""), arguments.get("top_k", 5))}


async def call_tool(arguments: dict, ctx: ToolContext) -> dict:
    """Straight proxy to mcp-hub's own call_tool — only mcp-hub-sourced
    search_tools results are invoked this way; a shell-hub-sourced result is
    invoked via shell_exec directly instead (see shell_hub.py's module
    docstring)."""
    return await mcp_hub.call_tool("call_tool", arguments)


async def lcm_grep(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_grep`.
    Session-scoped from ctx.session_key directly, same as every other
    session-scoped tool here — the model never supplies a session_key
    itself. mode/limit passed straight through with lcm.retrieval's own
    defaults (mode="pattern", limit=GREP_DEFAULT_LIMIT) when omitted."""
    pattern = arguments.get("pattern")
    if not isinstance(pattern, str) or not pattern:
        raise ValueError("lcm_grep requires a non-empty string 'pattern' argument")
    async with ctx.pool.acquire() as conn:
        return await lcm.grep(
            conn,
            ctx.session_key,
            pattern,
            mode=arguments.get("mode", "pattern"),
            limit=arguments.get("limit"),
        )


async def lcm_describe(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_describe`.
    Not session-scoped at the query level (a message_id/summary_id is
    already globally unique) — but every id a model could plausibly supply
    came from either lcm_grep's own output or a summary block lcm.assemble
    rendered into THIS session's context, so in practice it never crosses a
    session boundary; no explicit ctx.session_key check added on top of
    that, matching memory_search/memory_expand's own "unrestricted" stance
    just above."""
    id_arg = arguments.get("id")
    if not isinstance(id_arg, str) or not id_arg:
        raise ValueError("lcm_describe requires a non-empty string 'id' argument")
    async with ctx.pool.acquire() as conn:
        return await lcm.describe(conn, id_arg)


async def lcm_expand(arguments: dict, ctx: ToolContext) -> dict:
    """docs/components/context-slot.md's Memory-Access Tools — `lcm_expand`.
    Schema-level restriction to subagent turns only lives in llm.py
    (tools_schema_for) — this handler itself has no access-control check,
    same "enforced at the schema boundary, not re-checked in the handler"
    pattern shell_exec's own module docstring already documents for a
    different concern."""
    summary_id = arguments.get("summary_id")
    if not isinstance(summary_id, str) or not summary_id:
        raise ValueError("lcm_expand requires a non-empty string 'summary_id' argument")
    async with ctx.pool.acquire() as conn:
        return await lcm.expand(conn, summary_id)


async def _demo_echo_stub(arguments: dict, ctx: ToolContext) -> dict:
    """Reproduces the pre-real-tool-registry stub's exact behavior: a fixed
    ~4s simulated delay with heartbeats (real cancellation demonstrable), no
    actual work, just an echo of its own arguments. See module docstring —
    this exists purely for workflows/scenarios/'s existing demo tool names,
    not as a general fallback."""
    for _ in range(20):
        await asyncio.sleep(0.2)
        activity.heartbeat()
        if activity.is_cancelled():
            raise CancelledError("tool call cancelled")
    return {"echo_arguments": arguments}


_DEMO_TOOL_SPEC = ToolSpec(
    handler=_demo_echo_stub,
    heartbeat_interval_seconds=0.2,
    heartbeat_timeout_seconds=1.0,
    start_to_close_timeout_seconds=30.0,
)

TOOL_REGISTRY: dict[str, ToolSpec] = {
    "shell_exec": ToolSpec(
        handler=shell_exec,
        heartbeat_interval_seconds=3.0,
        heartbeat_timeout_seconds=10.0,
        start_to_close_timeout_seconds=300.0,
    ),
    # Tier B, matching shell_exec — a merge of many files is filesystem-
    # touching, chunkable work on the same PV, holds a session-directory
    # lease the same way, and needs the same heartbeat cadence for real
    # cancellation delivery mid-merge.
    "merge_subagent_output": ToolSpec(
        handler=merge_subagent_output,
        heartbeat_interval_seconds=3.0,
        heartbeat_timeout_seconds=10.0,
        start_to_close_timeout_seconds=300.0,
    ),
    "search": _DEMO_TOOL_SPEC,
    "slow_tool": _DEMO_TOOL_SPEC,
    "noop_tool": _DEMO_TOOL_SPEC,
    "memory_search": ToolSpec(
        handler=memory_search,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=30.0,
    ),
    "memory_expand": ToolSpec(
        handler=memory_expand,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=30.0,
    ),
    "search_tools": ToolSpec(
        handler=search_tools,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=30.0,
    ),
    "call_tool": ToolSpec(
        handler=call_tool,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=30.0,
    ),
    # lcm_grep/lcm_describe/lcm_expand (docs/components/context-slot.md's
    # Memory-Access Tools) — same no-cancellable-subprocess shape as
    # memory_search/memory_expand above (quick request/response, no lease),
    # but a local Postgres read against this tenant's own database, not a
    # network call to agent-brain — start_to_close is tightened to 15s
    # (vs. those tools' 30s) to reflect that real difference in expected
    # latency, not copied blindly from the network-tier tools next to it.
    "lcm_grep": ToolSpec(
        handler=lcm_grep,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=15.0,
    ),
    "lcm_describe": ToolSpec(
        handler=lcm_describe,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=15.0,
    ),
    "lcm_expand": ToolSpec(
        handler=lcm_expand,
        heartbeat_interval_seconds=5.0,
        heartbeat_timeout_seconds=15.0,
        start_to_close_timeout_seconds=15.0,
    ),
}
