"""Read/write helpers for the `skill_procedures` table
(docs/components/skill-subsystem.md, "Data model").

Phases 1–4: read current procedures (retrieval), upsert authored seeds,
write skill_candidates + EMA-update procedures (recording), the synthesis
writes (insert/version/notes/mark), and the co-occurrence graph
(update_cooccurrence / edge_weights). The cluster tables belong to phase 5.
"""

from __future__ import annotations

import json
import uuid
from dataclasses import dataclass, field

from .vectors import ema_blend

# EMA rates — docs/components/skill-subsystem.md, "EMA updates" / "Confidence".
CONFIDENCE_ALPHA = 0.2
TRIGGER_BETA = 0.15


@dataclass
class Procedure:
    id: str
    version: int
    title: str
    trigger_text: str
    trigger_embedding: list[float] | None
    body: list[dict]
    preconditions: list[str] = field(default_factory=list)
    done_criteria: list[str] = field(default_factory=list)
    notes: list[str] = field(default_factory=list)
    provenance: str = "authored"
    scope: str = "global"
    confidence: float = 0.25
    run_count: int = 0
    cluster_radius: float | None = None
    last_used_at: object | None = None  # datetime | None

    def render(self) -> str:
        """The procedure as prompt text — used both to size it for selection
        and as input to composition."""
        lines = [f"## {self.title}"]
        if self.preconditions:
            lines.append("Preconditions: " + "; ".join(self.preconditions))
        for i, step in enumerate(self.body, start=1):
            tool = step.get("tool_ref")
            suffix = f"  (tool: {tool})" if tool else ""
            lines.append(f"{i}. {step.get('instruction', '').strip()}{suffix}")
        if self.done_criteria:
            lines.append("Done when: " + "; ".join(self.done_criteria))
        for note in self.notes:
            lines.append(f"Note: {note}")
        return "\n".join(lines)


_COLUMNS = (
    "id, version, title, trigger_text, trigger_embedding, body, preconditions, "
    "done_criteria, notes, provenance, scope, confidence, run_count, cluster_radius, last_used_at"
)


def _row_to_procedure(row) -> Procedure:
    return Procedure(
        id=row["id"],
        version=row["version"],
        title=row["title"],
        trigger_text=row["trigger_text"],
        trigger_embedding=[float(x) for x in row["trigger_embedding"]]
        if row["trigger_embedding"] is not None
        else None,
        body=json.loads(row["body"]),
        preconditions=json.loads(row["preconditions"]),
        done_criteria=json.loads(row["done_criteria"]),
        notes=json.loads(row["notes"]),
        provenance=row["provenance"],
        scope=row["scope"],
        confidence=float(row["confidence"]),
        run_count=row["run_count"],
        cluster_radius=float(row["cluster_radius"]) if row["cluster_radius"] is not None else None,
        last_used_at=row["last_used_at"],
    )


async def current_procedures(db, scopes: tuple[str, ...]) -> list[Procedure]:
    """Every current (`valid_to IS NULL`) procedure whose scope is in `scopes`."""
    rows = await db.fetch(
        f"SELECT {_COLUMNS} FROM skill_procedures "
        "WHERE valid_to IS NULL AND scope = ANY($1::text[])",
        list(scopes),
    )
    return [_row_to_procedure(r) for r in rows]


async def procedures_by_ids(db, ids: list[str]) -> dict[str, Procedure]:
    """Current procedures keyed by id — used by ComposeSkill to load the
    bodies of the ids `SkillDiscover` staged."""
    if not ids:
        return {}
    rows = await db.fetch(
        f"SELECT {_COLUMNS} FROM skill_procedures "
        "WHERE valid_to IS NULL AND id = ANY($1::text[])",
        ids,
    )
    return {r["id"]: _row_to_procedure(r) for r in rows}


async def upsert_authored(db, spec: dict, embedding: list[float] | None) -> None:
    """Insert or replace an authored seed procedure (version 1). Idempotent —
    a re-run with the same `trigger_text` leaves the row (and any accumulated
    confidence / run_count) untouched; a changed `trigger_text` or a missing
    row replaces it with the fresh embedding at the authored prior."""
    existing = await db.fetchrow(
        "SELECT trigger_text FROM skill_procedures WHERE id = $1 AND provenance = 'authored'",
        spec["id"],
    )
    if existing is not None and existing["trigger_text"] == spec["trigger_text"]:
        return

    await db.execute(
        "DELETE FROM skill_procedures WHERE id = $1 AND provenance = 'authored'", spec["id"]
    )
    await db.execute(
        "INSERT INTO skill_procedures "
        "(id, version, title, trigger_text, trigger_embedding, body, preconditions, "
        " done_criteria, notes, provenance, scope, confidence, run_count) "
        "VALUES ($1, 1, $2, $3, $4, $5, $6, $7, $8, 'authored', $9, 0.7, 0)",
        spec["id"],
        spec["title"],
        spec["trigger_text"],
        embedding,
        json.dumps(spec.get("body", [])),
        json.dumps(spec.get("preconditions", [])),
        json.dumps(spec.get("done_criteria", [])),
        json.dumps(spec.get("notes", [])),
        spec.get("scope", "global"),
    )


# --- phase 2: recording -----------------------------------------------------


async def insert_candidate(
    db,
    turn_id: str,
    task_text: str,
    task_embedding: list[float] | None,
    transcript: str,
    outcome: str,
    required_correction: bool,
    composed_from: list[str],
) -> None:
    """One `skill_candidates` row. Idempotent per turn — a Temporal activity
    retry replaces the prior row for this turn rather than duplicating."""
    await db.execute("DELETE FROM skill_candidates WHERE turn_id = $1 AND synthesized_at IS NULL", turn_id)
    await db.execute(
        "INSERT INTO skill_candidates "
        "(turn_id, task_text, task_embedding, transcript, outcome, required_correction, composed_from) "
        "VALUES ($1, $2, $3, $4, $5, $6, $7)",
        turn_id,
        task_text,
        task_embedding,
        transcript,
        outcome,
        required_correction,
        json.dumps(composed_from),
    )


async def ema_update(
    db, procedure_id: str, reward: float, task_embedding: list[float] | None
) -> None:
    """Fold one turn's terminal reward into a composed procedure's current
    version — `confidence` always, `trigger_embedding` only on a positive
    reward (a failure shouldn't pull the retrieval key toward the failing
    task). `run_count` is bumped as an evidence-volume signal.
    docs/components/skill-subsystem.md, "EMA updates" + "Confidence"."""
    row = await db.fetchrow(
        "SELECT version, confidence, trigger_embedding FROM skill_procedures "
        "WHERE id = $1 AND valid_to IS NULL",
        procedure_id,
    )
    if row is None:
        return
    new_confidence = (1.0 - CONFIDENCE_ALPHA) * float(row["confidence"]) + CONFIDENCE_ALPHA * reward
    new_embedding = row["trigger_embedding"]
    if reward > 0.0 and task_embedding:
        current = [float(x) for x in row["trigger_embedding"]] if row["trigger_embedding"] is not None else None
        new_embedding = ema_blend(current, task_embedding, TRIGGER_BETA)
    await db.execute(
        "UPDATE skill_procedures SET confidence = $2, trigger_embedding = $3, "
        "run_count = run_count + 1, last_used_at = now(), updated_at = now() "
        "WHERE id = $1 AND version = $4",
        procedure_id,
        new_confidence,
        new_embedding,
        row["version"],
    )


# --- phase 3: synthesis ---------------------------------------------------------


@dataclass
class CandidateRow:
    id: str
    turn_id: str
    task_text: str
    task_embedding: list[float] | None
    transcript: str
    outcome: str
    required_correction: bool
    composed_from: list[str]


async def unsynthesized_candidates(db, limit: int = 200) -> list[CandidateRow]:
    """Oldest-first. Only rows with an embedding — an unembeddable candidate
    can't be assigned to a cluster, so there's nothing synthesis can do with
    it; it stays queued in case the embedding backend is configured later."""
    rows = await db.fetch(
        "SELECT id, turn_id, task_text, task_embedding, transcript, outcome, "
        "required_correction, composed_from FROM skill_candidates "
        "WHERE synthesized_at IS NULL AND task_embedding IS NOT NULL "
        "ORDER BY created_at LIMIT $1",
        limit,
    )
    return [
        CandidateRow(
            id=str(r["id"]),
            turn_id=r["turn_id"],
            task_text=r["task_text"],
            task_embedding=[float(x) for x in r["task_embedding"]] if r["task_embedding"] is not None else None,
            transcript=r["transcript"],
            outcome=r["outcome"],
            required_correction=r["required_correction"],
            composed_from=json.loads(r["composed_from"]),
        )
        for r in rows
    ]


async def mark_synthesized(db, candidate_ids: list[str]) -> None:
    if not candidate_ids:
        return
    await db.execute(
        "UPDATE skill_candidates SET synthesized_at = now() WHERE id = ANY($1::uuid[])",
        candidate_ids,
    )


async def insert_learned(
    db,
    spec: dict,
    embedding: list[float] | None,
    source_ids: list[str],
    cluster_radius: float | None = None,
) -> str:
    """A brand-new learned procedure (version 1) at the skeptical prior."""
    procedure_id = "learned:" + uuid.uuid4().hex[:16]
    await db.execute(
        "INSERT INTO skill_procedures "
        "(id, version, title, trigger_text, trigger_embedding, body, preconditions, "
        " done_criteria, notes, provenance, source_ids, scope, confidence, run_count, cluster_radius) "
        "VALUES ($1, 1, $2, $3, $4, $5, $6, $7, $8, 'learned', $9, 'global', 0.25, 0, $10)",
        procedure_id,
        spec["title"],
        spec["trigger_text"],
        embedding,
        json.dumps(spec.get("body", [])),
        json.dumps(spec.get("preconditions", [])),
        json.dumps(spec.get("done_criteria", [])),
        json.dumps(spec.get("notes", [])),
        json.dumps(source_ids),
        cluster_radius,
    )
    return procedure_id


async def new_version(
    db,
    procedure_id: str,
    spec: dict,
    embedding: list[float] | None,
    source_ids: list[str],
    cluster_radius: float | None = None,
) -> None:
    """Supersede the current version with a re-synthesized body. confidence and
    trigger_embedding carry forward (they were EMA'd all along, not reset) —
    unless a fresh embedding is supplied for the new trigger_text."""
    row = await db.fetchrow(
        "SELECT version, confidence, trigger_embedding, run_count, scope, cluster_radius, last_used_at "
        "FROM skill_procedures WHERE id = $1 AND valid_to IS NULL",
        procedure_id,
    )
    if row is None:
        return
    next_version = row["version"] + 1
    await db.execute(
        "UPDATE skill_procedures SET valid_to = now(), superseded_by = $2 WHERE id = $1 AND version = $3",
        procedure_id,
        f"{procedure_id}:{next_version}",
        row["version"],
    )
    carried = row["trigger_embedding"]
    carried = [float(x) for x in carried] if carried is not None else None
    await db.execute(
        "INSERT INTO skill_procedures "
        "(id, version, title, trigger_text, trigger_embedding, body, preconditions, "
        " done_criteria, notes, provenance, source_ids, scope, confidence, run_count, "
        " cluster_radius, last_used_at) "
        "VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'learned', $10, $11, $12, $13, $14, $15)",
        procedure_id,
        next_version,
        spec["title"],
        spec["trigger_text"],
        embedding or carried,
        json.dumps(spec.get("body", [])),
        json.dumps(spec.get("preconditions", [])),
        json.dumps(spec.get("done_criteria", [])),
        json.dumps(spec.get("notes", [])),
        json.dumps(source_ids),
        row["scope"],
        float(row["confidence"]),
        row["run_count"],
        cluster_radius if cluster_radius is not None else row["cluster_radius"],
        row["last_used_at"],
    )


async def append_notes(db, procedure_id: str, new_notes: list[str]) -> None:
    """Add cautions to the current version's notes without a version bump —
    the body is unchanged. Deduped against what's already there."""
    row = await db.fetchrow(
        "SELECT version, notes FROM skill_procedures WHERE id = $1 AND valid_to IS NULL",
        procedure_id,
    )
    if row is None or not new_notes:
        return
    existing = json.loads(row["notes"])
    merged = existing + [n for n in new_notes if n and n not in existing]
    if merged == existing:
        return
    await db.execute(
        "UPDATE skill_procedures SET notes = $2, updated_at = now() WHERE id = $1 AND version = $3",
        procedure_id,
        json.dumps(merged),
        row["version"],
    )


# --- phase 4: co-occurrence ---------------------------------------------------


COOCCUR_GAMMA = 0.15
COOCCUR_FLOOR = 0.02          # drop edges whose EMA has decayed below this
COOCCUR_FORGET_DAYS = 90


def _pairs(within: list[str], across: list[str]) -> set[tuple[str, str]]:
    a = sorted(set(within))
    b = set(across) - set(a)
    out: set[tuple[str, str]] = set()
    for i, x in enumerate(a):
        for y in a[i + 1 :]:
            out.add((x, y))
    for x in a:
        for y in b:
            out.add(tuple(sorted((x, y))))
    return out


async def session_composed_procedure_ids(db, session_key: str, exclude_turn_id: str) -> list[str]:
    """Procedure ids composed into any *other* top-level turn of the same
    session — the `R` set for co-occurrence (skill-subsystem.md, "The
    co-occurrence graph"). Cross-session / project-scoped linking needs
    project scope wired, which isn't yet."""
    rows = await db.fetch(
        "SELECT DISTINCT tr.metadata->>'procedure_id' AS pid "
        "FROM turn_retrieval tr JOIN turns t ON tr.turn_id = t.turn_id "
        "WHERE t.parent_id = $1 AND t.parent_type = 'session' AND tr.kind = 'skill' "
        "  AND tr.turn_id <> $2 AND tr.metadata->>'procedure_id' IS NOT NULL",
        session_key,
        exclude_turn_id,
    )
    return [r["pid"] for r in rows if r["pid"]]


async def update_cooccurrence(db, this_turn_ids: list[str], recent_ids: list[str], reward: float) -> None:
    """EMA-update the edges for one turn: all `this_turn_ids` × `this_turn_ids`
    pairs plus `this_turn_ids` × `recent_ids`. A brand-new edge starts at
    `gamma * reward` (EMA from zero — a pairing has to prove itself over
    repeated co-occurrence), then decays toward each subsequent reward."""
    pairs = _pairs(this_turn_ids, recent_ids)
    if not pairs:
        return
    for a, b in pairs:
        await db.execute(
            "INSERT INTO skill_cooccurrence (proc_a, proc_b, edge, last_seen_at) "
            "VALUES ($1, $2, $3, now()) "
            "ON CONFLICT (proc_a, proc_b) DO UPDATE SET "
            "  edge = (1 - $4) * skill_cooccurrence.edge + $4 * $5, last_seen_at = now()",
            a,
            b,
            COOCCUR_GAMMA * reward,
            COOCCUR_GAMMA,
            reward,
        )
    await db.execute(
        "DELETE FROM skill_cooccurrence "
        "WHERE edge < $1 OR last_seen_at < now() - make_interval(days => $2)",
        COOCCUR_FLOOR,
        COOCCUR_FORGET_DAYS,
    )


async def edge_weights(db, procedure_ids: list[str]) -> dict[frozenset, float]:
    """`{frozenset({a, b}): edge}` for every co-occurrence edge among
    `procedure_ids`. The retrieval `w_co` input."""
    ids = sorted(set(procedure_ids))
    if len(ids) < 2:
        return {}
    rows = await db.fetch(
        "SELECT proc_a, proc_b, edge FROM skill_cooccurrence "
        "WHERE proc_a = ANY($1::text[]) AND proc_b = ANY($1::text[])",
        ids,
    )
    return {frozenset((r["proc_a"], r["proc_b"])): float(r["edge"]) for r in rows}
