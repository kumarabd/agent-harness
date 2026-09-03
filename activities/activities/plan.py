"""The checkpoint ledger — request pipeline step 8
(docs/components/request-pipeline/08-planning.md).

REVISED 2026-09-03 (08-planning.md REVISION, Phase 3 slice C — plan-and-execute):
a Deliberate task-run is a `PlanWorkflow` (decision B — no `episodes` table).
The ledger is a **PLAN.md file** on the tenant PV
(`$SESSION_ROOT/session/<session_key>/plans/<plan_id>/PLAN.md`) — greppable by
shell tools and by any delegated Claude Code working the same task, and the
exact artifact a human edits at the approval gate.

Lifecycle:
  - the **planning turn** (`ModelCall` in `planning_mode`) calls the
    `propose_plan` meta-tool → peeled by `ModelCall` → `seed()` writes PLAN.md;
  - `PlanWorkflow` walks the ledger one checkpoint at a time via
    `NextCheckpoint` (plan_resolve.py); each **checkpoint turn** does its step
    and calls `checkpoint_done` → peeled by `ModelCall` → `apply_checkpoint_done()`
    marks that checkpoint and optionally replaces the pending tail;
  - `RecordSkill` reads the final state at plan close.

File format — one checkpoint per line, optional indented `done_when:` / `note:`:

    # Plan
    status: executing

    - cp1 [x] Locate the failing test
    - cp2 [ ] Reproduce the failure
          done_when: the failure reproduces locally

    marks: [x] done  [-] skipped  [~] revised  [ ] pending

The I/O functions take a `plan_id` and do file I/O directly — every caller
(`ModelCall`, `NextCheckpoint`, `RecordSkill`) is a tenant-worker activity with
the PV mounted. No locking: `PlanWorkflow` serialises its turns and `ModelCall`
runs serially within a turn.
"""

from __future__ import annotations

import os
import re
from dataclasses import dataclass

from . import ids

PROPOSE_PLAN_TOOL_NAME = "propose_plan"
CHECKPOINT_DONE_TOOL_NAME = "checkpoint_done"

_REPORTABLE = {"done", "skipped", "revised"}
_TERMINAL = {"done", "skipped"}

_SESSION_ROOT_ENV = "SESSION_ROOT"
_DEFAULT_SESSION_ROOT = "/tmp/agent-harness-sessions"

_MARK_W = {"done": "[x]", "skipped": "[-]", "revised": "[~]", "pending": "[ ]", "active": "[ ]"}
_MARK_R = {"x": "done", "-": "skipped", "~": "revised", " ": "pending", ">": "pending"}
_CP_LINE = re.compile(r"^- (\S+) \[([x~> -])\] (.*)$")
_ATTR_LINE = re.compile(r"^\s+(done_when|note|complex): (.*)$")
_TRUE = {"true", "yes", "1"}


@dataclass
class Checkpoint:
    cp_id: str
    checkpoint: int
    intent: str
    done_when: str
    status: str
    note: str | None
    # 3C-iii — the planning model flagged this step as itself a multi-step
    # subtask; PlanWorkflow runs it as a nested PlanWorkflow (docs/components/
    # request-pipeline/08-planning.md).
    complex: bool = False


def _plan_path(plan_id: str) -> str:
    root = os.environ.get(_SESSION_ROOT_ENV, _DEFAULT_SESSION_ROOT)
    session_key = ids.session_key_of(plan_id)
    safe = plan_id.replace(":", "_")
    return os.path.join(root, "session", session_key, "plans", safe, "PLAN.md")


def _read_file(plan_id: str) -> list[Checkpoint]:
    try:
        text = open(_plan_path(plan_id), encoding="utf-8").read()
    except FileNotFoundError:
        return []
    cps: list[Checkpoint] = []
    for line in text.splitlines():
        m = _CP_LINE.match(line)
        if m:
            cp_id, mark, intent = m.group(1), m.group(2), m.group(3).strip()
            cps.append(
                Checkpoint(
                    cp_id=cp_id,
                    checkpoint=len(cps) + 1,
                    intent=intent,
                    done_when="",
                    status=_MARK_R.get(mark, "pending"),
                    note=None,
                )
            )
            continue
        a = _ATTR_LINE.match(line)
        if a and cps:
            key, val = a.group(1), a.group(2).strip()
            if key == "done_when":
                cps[-1].done_when = val
            elif key == "complex":
                cps[-1].complex = val.lower() in _TRUE
            else:
                cps[-1].note = val or None
    return cps


def _write_file(plan_id: str, cps: list[Checkpoint]) -> None:
    path = _plan_path(plan_id)
    os.makedirs(os.path.dirname(path), exist_ok=True)
    status = "complete" if all_terminal(cps) else "executing"
    lines = ["# Plan", "status: " + status, ""]
    for cp in cps:
        lines.append(f"- {cp.cp_id} {_MARK_W.get(cp.status, '[ ]')} {cp.intent}")
        if cp.done_when:
            lines.append(f"      done_when: {cp.done_when}")
        if cp.complex:
            lines.append("      complex: true")
        if cp.note:
            lines.append(f"      note: {cp.note}")
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")
    os.replace(tmp, path)


async def seed(plan_id: str, checkpoints: list[dict]) -> int:
    """Write the ledger from an ordered list of `{intent, done_when?, complex?}` —
    called by the planning turn (via `propose_plan`) and again by a mid-plan re-plan.
    Merges onto an existing file (keeps the status of matching cp ids) so a
    re-plan preserves completed checkpoints rather than clobbering progress.

    The approval gate is PlanWorkflow's concern, not this file's: `propose_plan`'s
    `needs_approval` rides out on ModelCallOutput → TurnResult, and PlanWorkflow
    parks on a UserInputRequestWorkflow before it ever calls NextCheckpoint."""
    existing = {cp.cp_id: cp for cp in _read_file(plan_id)}
    out: list[Checkpoint] = []
    for i, cp in enumerate(checkpoints, start=1):
        intent = str(cp.get("intent", "")).strip()
        if not intent:
            continue
        cp_id = f"cp{i}"
        prev = existing.get(cp_id)
        out.append(
            Checkpoint(
                cp_id=cp_id,
                checkpoint=len(out) + 1,
                intent=intent,
                done_when=str(cp.get("done_when", "") or "").strip(),
                status=prev.status if prev else "pending",
                note=prev.note if prev else None,
                complex=bool(cp.get("complex", False)),
            )
        )
    if not out:
        return 0
    _write_file(plan_id, out)
    return len(out)


async def apply_checkpoint_done(plan_id: str, calls: list[dict]) -> int:
    """Apply the model's `checkpoint_done` reports from a checkpoint turn. Each
    call is `{checkpoint_id, status, note?, revised_tail?}`:

      - `status` in {done, skipped, revised} marks that checkpoint;
      - `note` annotates it (kept on the line);
      - `revised_tail` (a list of `{intent, done_when?, complex?}`) replaces
        every still-pending checkpoint *after* this one — the checkpoint turn
        re-planning the remainder given what it found. Terminal checkpoints
        (already done/skipped) are never touched.

    Returns the number of calls applied."""
    if not calls:
        return 0
    cps = _read_file(plan_id)
    by_id = {cp.cp_id: cp for cp in cps}
    applied = 0
    for u in calls:
        cp_id = str(u.get("checkpoint_id", "") or "").strip()
        if not cp_id or cp_id not in by_id:
            continue
        target = by_id[cp_id]
        status = str(u.get("status", "") or "").strip().lower()
        if status not in _REPORTABLE:
            status = "done"
        target.status = status
        note = str(u.get("note", "") or "").strip() or None
        if note:
            target.note = note

        tail = u.get("revised_tail")
        if isinstance(tail, list) and tail:
            idx = cps.index(target)
            head = cps[: idx + 1]
            # keep any terminal checkpoints that sat after this one, drop pending
            kept_terminal = [cp for cp in cps[idx + 1 :] if cp.status in _TERMINAL]
            new_tail: list[Checkpoint] = []
            base = len(head) + len(kept_terminal)
            for j, item in enumerate(tail, start=1):
                intent = str(item.get("intent", "") or "").strip()
                if not intent:
                    continue
                new_tail.append(
                    Checkpoint(
                        cp_id=f"cp{base + j}",
                        checkpoint=base + j,
                        intent=intent,
                        done_when=str(item.get("done_when", "") or "").strip(),
                        status="pending",
                        note=None,
                        complex=bool(item.get("complex", False)),
                    )
                )
            cps = head + kept_terminal + new_tail
            by_id = {cp.cp_id: cp for cp in cps}
        applied += 1
    if applied:
        for i, cp in enumerate(cps, start=1):
            cp.checkpoint = i
        _write_file(plan_id, cps)
    return applied


async def read(plan_id: str) -> list[Checkpoint]:
    return _read_file(plan_id)


def all_terminal(checkpoints: list[Checkpoint]) -> bool:
    """True when the ledger has at least one checkpoint and every one is
    done/skipped — the plan is finished (episode-lifecycle.md `plan_complete`)."""
    return bool(checkpoints) and all(cp.status in _TERMINAL for cp in checkpoints)


_MARK = {"done": "[x]", "skipped": "[-]", "revised": "[~]"}


def render_block(checkpoints: list[Checkpoint]) -> str:
    """The progress block spliced into the prompt. `[>]` marks the first
    non-terminal checkpoint — the renderer owns "which is active"."""
    if not checkpoints:
        return ""
    active_marked = False
    lines = ["Plan progress — follow it where it fits, revise it where the task diverges:"]
    for cp in checkpoints:
        if cp.status in _TERMINAL:
            mark = _MARK[cp.status]
        elif not active_marked:
            mark, active_marked = "[>]", True
        else:
            mark = "[ ]"
        line = f"  {mark} {cp.checkpoint}. {cp.intent}"
        if cp.status == "revised" and cp.note:
            line += f"  (revised: {cp.note})"
        elif cp.status == "revised":
            line += "  (revised)"
        elif cp.note:
            line += f"  (note: {cp.note})"
        lines.append(line)
    return "\n".join(lines)


def render_final(checkpoints: list[Checkpoint]) -> str:
    """The final ledger as text for RecordSkill's trajectory — the effective
    procedure the run followed."""
    if not checkpoints:
        return ""
    lines = ["PLAN (final state):"]
    for cp in checkpoints:
        suffix = f" [{cp.status}]" if cp.status != "pending" else " [not reached]"
        lines.append(f"  {cp.checkpoint}. {cp.intent}{suffix}" + (f" — {cp.note}" if cp.note else ""))
    return "\n".join(lines)


def split_propose_plan(raw_tool_calls: list[dict]) -> tuple[dict | None, list[dict]]:
    """Partition a planning / re-plan turn's tool calls into (proposal, rest).
    `proposal` is `{"checkpoints": [...], "needs_approval": bool}` from the LAST
    `propose_plan` call, or None if the model didn't propose one."""
    proposal: dict | None = None
    rest: list[dict] = []
    for tc in raw_tool_calls:
        if tc.get("name") == PROPOSE_PLAN_TOOL_NAME:
            args = tc.get("arguments") or {}
            items = args.get("checkpoints")
            proposal = {
                "checkpoints": [c for c in items if isinstance(c, dict)] if isinstance(items, list) else [],
                "needs_approval": bool(args.get("needs_approval", False)),
            }
            continue
        rest.append(tc)
    return proposal, rest


def split_checkpoint_done(raw_tool_calls: list[dict]) -> tuple[list[dict], list[dict]]:
    """Partition a checkpoint turn's tool calls into (checkpoint_done calls, rest)."""
    done: list[dict] = []
    rest: list[dict] = []
    for tc in raw_tool_calls:
        if tc.get("name") == CHECKPOINT_DONE_TOOL_NAME:
            args = tc.get("arguments") or {}
            if isinstance(args, dict):
                done.append(args)
            continue
        rest.append(tc)
    return done, rest
