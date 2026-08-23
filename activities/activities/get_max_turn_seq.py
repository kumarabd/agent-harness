"""GetMaxTurnSeq activity — the real body for what coordinator.go's own
comment already documented as the intended design ("real design recomputes
MAX(turn_seq)+1 from Postgres on startup") but never implemented, stubbing
turnSeq at 0 instead. That stub meant every fresh CoordinatorWorkflow
execution (i.e. any time the prior one idles out, idleTTL, and a later
message starts a new one) reused turn_id "...:turn:1" again regardless of
how many real turns the session already had — a real turn_id collision, not
a hypothetical one (found directly: 13 separately-sent real messages all
landed under one reused turn_id instead of turn:1..13).

The Coordinator is workflow code — it can't query Postgres directly without
breaking Temporal's determinism boundary — so this activity exists purely
to do that one lookup on its behalf, once per coordinator startup, mirroring
the exact query workflows/cmd/starter/main.go already uses client-side for
the same reason (predicting the coordinator's own turn_id math).
"""

from __future__ import annotations

from temporalio import activity


class GetMaxTurnSeqActivity:
    def __init__(self, pool):
        self._pool = pool

    @activity.defn(name="GetMaxTurnSeq")
    async def __call__(self, session_key: str) -> int:
        max_seq = await self._pool.fetchval(
            "SELECT MAX(turn_seq) FROM turns WHERE parent_id = $1 AND parent_type = 'session'",
            session_key,
        )
        return max_seq or 0
