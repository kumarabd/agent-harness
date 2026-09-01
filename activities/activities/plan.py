"""The living checkpoint ledger — request pipeline step 8
(docs/components/request-pipeline/08-planning.md).

`ComposeSkill` seeds a `turn_plan` from the merged procedure's ordered steps.
The model reports advancement through the `plan_progress` meta-tool; `ModelCall`
peels those calls off the response and applies them here (no separate activity —
same shape as `declare_next_step_hint`, which the providers strip inline).
`build_conversation` renders the current state into a compact progress block
every `ModelCall`. `RecordSkillOutcome` reads the final state for synthesis.

No I/O helpers beyond the four below; every read/write takes an open
connection so the caller controls the transaction.
"""

from __future__ import annotations

from dataclasses import dataclass

PLAN_PROGRESS_TOOL_NAME = "plan_progress"

# Statuses the model may report. "active"/"pending" are managed by the renderer,
# not the model — it only ever says a checkpoint is done/skipped/revised.
_REPORTABLE = {"done", "skipped", "revised"}
_TERMINAL = {"done", "skipped"}


@dataclass
class Checkpoint:
    cp_id: str
    checkpoint: int
    intent: str
    done_when: str
    status: str
    note: str | None


async def seed(conn, turn_id: str, checkpoints: list[dict]) -> int:
    """Write the initial ledger. `checkpoints` is an ordered list of
    `{intent, done_when?}`. Idempotent per (turn_id, cp_id) so a ComposeSkill
    retry re-writes rather than colliding; a re-seed after progress was recorded
    would clobber status, so callers only seed once (ComposeSkill runs once)."""
    rows = [
        (turn_id, f"cp{i}", i, str(cp.get("intent", "")).strip(), str(cp.get("done_when", "") or "").strip())
        for i, cp in enumerate(checkpoints, start=1)
        if str(cp.get("intent", "")).strip()
    ]
    if not rows:
        return 0
    await conn.executemany(
        "INSERT INTO turn_plan (turn_id, cp_id, checkpoint, intent, done_when) "
        "VALUES ($1, $2, $3, $4, $5) "
        "ON CONFLICT (turn_id, cp_id) DO UPDATE SET "
        "  checkpoint = EXCLUDED.checkpoint, intent = EXCLUDED.intent, "
        "  done_when = EXCLUDED.done_when, updated_at = now()",
        rows,
    )
    return len(rows)


async def apply_progress(conn, turn_id: str, updates: list[dict]) -> int:
    """Apply the model's `plan_progress` reports. Each update is
    `{checkpoint_id, status, note?}` for an existing checkpoint, or
    `{checkpoint_id, intent, status?, note?}` to append a step the model added
    mid-turn (unknown id + an intent). Unknown id with no intent is ignored.
    Returns the number applied."""
    if not updates:
        return 0
    existing = {
        r["cp_id"]: r["checkpoint"]
        for r in await conn.fetch("SELECT cp_id, checkpoint FROM turn_plan WHERE turn_id = $1", turn_id)
    }
    next_ord = (max(existing.values()) + 1) if existing else 1
    applied = 0
    for u in updates:
        cp_id = str(u.get("checkpoint_id", "") or "").strip()
        if not cp_id:
            continue
        status = str(u.get("status", "") or "").strip().lower()
        note = str(u.get("note", "") or "").strip() or None
        if cp_id in existing:
            if status not in _REPORTABLE:
                continue
            await conn.execute(
                "UPDATE turn_plan SET status = $3, note = COALESCE($4, note), updated_at = now() "
                "WHERE turn_id = $1 AND cp_id = $2",
                turn_id,
                cp_id,
                status,
                note,
            )
            applied += 1
        else:
            intent = str(u.get("intent", "") or "").strip()
            if not intent:
                continue
            await conn.execute(
                "INSERT INTO turn_plan (turn_id, cp_id, checkpoint, intent, status, note) "
                "VALUES ($1, $2, $3, $4, $5, $6) "
                "ON CONFLICT (turn_id, cp_id) DO NOTHING",
                turn_id,
                cp_id,
                next_ord,
                intent,
                status if status in _REPORTABLE else "pending",
                note,
            )
            existing[cp_id] = next_ord
            next_ord += 1
            applied += 1
    return applied


async def read(conn, turn_id: str) -> list[Checkpoint]:
    rows = await conn.fetch(
        "SELECT cp_id, checkpoint, intent, done_when, status, note FROM turn_plan "
        "WHERE turn_id = $1 ORDER BY checkpoint, cp_id",
        turn_id,
    )
    return [
        Checkpoint(
            cp_id=r["cp_id"],
            checkpoint=r["checkpoint"],
            intent=r["intent"],
            done_when=r["done_when"],
            status=r["status"],
            note=r["note"],
        )
        for r in rows
    ]


_MARK = {"done": "[x]", "skipped": "[-]", "revised": "[~]"}


def render_block(checkpoints: list[Checkpoint]) -> str:
    """The progress block spliced into the prompt. `[>]` marks the first
    checkpoint that isn't done/skipped — the renderer owns "which is active",
    the model never reports it."""
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
    """The final ledger as text for the synthesis trajectory
    (RecordSkillOutcome). Ordinal, intent, terminal status, and any note —
    this is the effective procedure the run followed."""
    if not checkpoints:
        return ""
    lines = ["PLAN (final state):"]
    for cp in checkpoints:
        suffix = f" [{cp.status}]" if cp.status != "pending" else " [not reached]"
        lines.append(f"  {cp.checkpoint}. {cp.intent}{suffix}" + (f" — {cp.note}" if cp.note else ""))
    return "\n".join(lines)


def split_progress_calls(raw_tool_calls: list[dict]) -> tuple[list[dict], list[dict]]:
    """Partition a response's tool calls into (plan_progress updates, the rest).
    A `plan_progress` call carries `{"updates": [...]}`; a malformed one
    contributes no updates but is still removed from the tool stream."""
    updates: list[dict] = []
    rest: list[dict] = []
    for tc in raw_tool_calls:
        if tc.get("name") == PLAN_PROGRESS_TOOL_NAME:
            args = tc.get("arguments") or {}
            items = args.get("updates")
            if isinstance(items, list):
                updates.extend(u for u in items if isinstance(u, dict))
            continue
        rest.append(tc)
    return updates, rest
