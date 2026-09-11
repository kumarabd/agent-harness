#!/usr/bin/env bash
# Live-cluster test for the cancel/stop primitive
# (docs/components/gateway/first-party-plan.md). Not a run_all.sh
# SCENARIOS/CHAINED_PAIRS entry — run_scenario.sh's harness always submits a
# scenario JSON as a NewMessage-shaped signal; a cancel is a structurally
# different, payload-less signal (turn.go's CancelSignalName), so this is its
# own small script rather than forcing that shape to fit.
#
# Flow: start cancel-initial (a gated `shell_exec sleep 15`) via
# run_scenario.sh in the background, wait for it to actually be mid-sleep
# (approved + running), send a Cancel via the starter's own -cancel flag
# (exactly what core.CancelActiveTurn does in production), then assert the
# turn ended 'cancelled' — not 'completed' — with no further ModelCall.
#
# Usage: workflows/scenarios/test_cancel.sh
set -uo pipefail

NAMESPACE=agents
PG_POD=abishekk-postgresql-0
PG_USER=agent_harness
PG_DB=agent_harness
TEMPORAL_NAMESPACE=abishekk

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
STARTER_BIN="$REPO_ROOT/.build/starter"

pg() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
fail() { echo "FAIL: $1" >&2; exit 1; }
ok() { echo "  ok: $1"; }

if ! nc -z localhost 17233 2>/dev/null; then
  kubectl port-forward -n core svc/temporal-frontend 17233:7233 >/dev/null 2>&1 &
  TEMPORAL_FWD_PID=$!
  trap '[ -n "${TEMPORAL_FWD_PID:-}" ] && kill "$TEMPORAL_FWD_PID" 2>/dev/null' EXIT
fi
for _ in $(seq 1 20); do nc -z localhost 17233 2>/dev/null && break; sleep 0.5; done

mkdir -p "$REPO_ROOT/.build"
if [ ! -x "$STARTER_BIN" ] || [ "$REPO_ROOT/workflows/cmd/starter/main.go" -nt "$STARTER_BIN" ]; then
  (cd "$REPO_ROOT/workflows" && go build -o "$STARTER_BIN" ./cmd/starter)
fi

SESSION_KEY="test:scenario:cancel:$(date +%s)-$$"
ROOT_TURN_ID="${SESSION_KEY}:turn:1"

echo "=== cancel primitive ==="
echo "session_key: $SESSION_KEY"

echo "--- starting cancel-initial (bg) ---"
bash "$SCRIPT_DIR/run_scenario.sh" cancel-initial "$SESSION_KEY" >/tmp/cancel-initial.$$.log 2>&1 &
INITIAL_PID=$!

echo "--- waiting for the gated shell_exec to be minted and approved ---"
# tool_calls has no distinct "running" status (pending covers both
# "not yet started" and "currently executing" — see model_call.py's own
# comment on why); wait for the mint (a real tool_call_id exists) and give
# the auto-approver + activity dispatch a moment, mirroring the
# interrupt-initial/-followup pair's own timing (run_all.sh: sleep 3 between
# starting -initial and firing the interrupt). sleep 15 is comfortably long
# enough either way.
MINTED=0
for _ in $(seq 1 30); do
  n="$(pg "SELECT count(*) FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'" 2>/dev/null || true)"
  [ "${n:-0}" -ge 1 ] 2>/dev/null && { MINTED=1; break; }
  sleep 1
done
[ "$MINTED" = "1" ] || fail "shell_exec tool_call was never minted"
sleep 5
ok "gated shell_exec minted and (auto-)approved"

echo "--- sending Cancel ---"
TEMPORAL_ADDRESS=localhost:17233 TEMPORAL_NAMESPACE="$TEMPORAL_NAMESPACE" \
  "$STARTER_BIN" -session "$SESSION_KEY" -cancel || fail "starter -cancel failed"

echo "--- waiting for the root turn to reach a terminal status ---"
STATUS=""
for _ in $(seq 1 30); do
  STATUS="$(pg "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'" 2>/dev/null || true)"
  case "$STATUS" in completed|failed|cancelled) break ;; esac
  sleep 1
done
wait "$INITIAL_PID" 2>/dev/null || true

[ "$STATUS" = "cancelled" ] || fail "root turn status is '$STATUS', expected 'cancelled'"
ok "turn ended with status='cancelled' (not 'completed')"

tc="$(pg "SELECT tool_name || '|' || status FROM tool_calls WHERE parent_id = '$ROOT_TURN_ID' AND tool_name = 'shell_exec'")"
[ "$tc" = "shell_exec|cancelled" ] || fail "shell_exec tool_call is '$tc', expected 'shell_exec|cancelled'"
ok "the in-flight tool call was cancelled"

n_assistant="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND role = 'assistant'")"
[ "${n_assistant:-0}" -le 1 ] || fail "expected at most 1 assistant message (the initial one), found $n_assistant — a cancel must not trigger another ModelCall"
ok "no further ModelCall happened after the cancel (assistant message count: ${n_assistant:-0})"

n_bad="$(pg "SELECT count(*) FROM messages WHERE parent_id = '$ROOT_TURN_ID' AND content ILIKE '%must never be used%'")"
[ "${n_bad:-0}" = "0" ] || fail "the second scripted response was consumed — cancel fell back to an ordinary interrupt fold-in"
ok "the second scripted response (only reachable via another ModelCall) was never consumed"

echo "PASS: cancel primitive (session=$SESSION_KEY)"
