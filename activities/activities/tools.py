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
"""

from __future__ import annotations

import asyncio
import os
import signal
from dataclasses import dataclass
from typing import Awaitable, Callable

import asyncpg
from temporalio import activity
from temporalio.exceptions import CancelledError

from . import leases

_SESSION_ROOT_ENV = "SESSION_ROOT"
# Local-dev fallback; real deployments set SESSION_ROOT to match the Helm
# chart's PV mount (deploy/helm/agent-harness-tenant's activityWorker mounts
# the session filesystem tree at /sessions).
_DEFAULT_SESSION_ROOT = "/tmp/agent-harness-sessions"

# Bytes, not characters - applied per stream (stdout/stderr independently).
# The full claim-check large-payload route through the PV (rather than
# Postgres) is still open in docs/components/session-filesystem.md (no
# numeric large-vs-small threshold specified) - out of scope here; this is
# just a safety cap so a runaway command can't push an unbounded blob into
# the tool_calls.result jsonb column.
_MAX_OUTPUT_BYTES = 4096


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
    unique lease-holder identity."""

    pool: asyncpg.Pool
    session_key: str
    fs_path: str
    session_dir: str
    holder_id: str
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


def _truncate(data: bytes) -> tuple[str, bool]:
    text = data.decode("utf-8", errors="replace")
    if len(text.encode("utf-8")) <= _MAX_OUTPUT_BYTES:
        return text, False
    truncated_bytes = text.encode("utf-8")[:_MAX_OUTPUT_BYTES]
    return truncated_bytes.decode("utf-8", errors="replace"), True


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

    stdout, stdout_truncated = _truncate(stdout_bytes)
    stderr, stderr_truncated = _truncate(stderr_bytes)
    return {
        "exit_code": proc.returncode,
        "stdout": stdout,
        "stderr": stderr,
        "truncated": stdout_truncated or stderr_truncated,
    }


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
    "search": _DEMO_TOOL_SPEC,
    "slow_tool": _DEMO_TOOL_SPEC,
    "noop_tool": _DEMO_TOOL_SPEC,
}
