#!/usr/bin/env bash
# Lightweight scenario runner — submits a scripted scenario against the
# ALREADY-DEPLOYED, live cluster workers (loop-worker + tenant-worker poll
# `agent-loop` regardless of who submits work onto it). Deliberately NOT
# deep-conversation/run.sh's heavier shape: no local worker binaries, no
# scaling cluster Deployments to 0. The reason-act loop is fixture-scripted
# (`_test_scripted_responses`), but the pre-LLM pipeline (classify / route /
# discover / compose) makes real fast/medium-tier calls per turn, and the
# `real-assembly` scenario deliberately runs one real reason-act call too —
# see README.md's "Not zero-cost any more". A few cents per suite run.
#
# Requires:
#   - kubectl context pointed at the right cluster
#   - a port-forward to Temporal frontend already running:
#       kubectl port-forward -n core svc/temporal-frontend 17233:7233 &
#   - `go build -o .build/starter ./cmd/starter` (this script builds it
#     itself if the binary is missing or the source is newer)
#   - every Postgres read/write goes through `kubectl exec ... psql`
#     server-side (see pg_query/pg_exec below) — the password never
#     touches this script or a local shell variable, same convention this
#     whole project uses. No local Postgres port-forward needed at all.
#
# Usage:
#   workflows/scenarios/run_scenario.sh <scenario-name> [session-key]
#
#   scenario-name: basename of workflows/scenarios/<name>.json (no
#     extension) — e.g. "spawn-subagent-nested-valid".
#   session-key: defaults to "test:scenario:<name>:<random-suffix>" —
#     always "test:"-prefixed so a leftover session is trivially
#     identifiable and safe to bulk-delete later (see cleanup_test_data.sh).
#
# Looks for two optional companion files next to <name>.json:
#   <name>.setup.sql  — applied via psql BEFORE the scenario runs, with
#     {{SESSION_KEY}} substituted for the real session key throughout —
#     for scenarios needing pre-existing Postgres state (e.g.
#     lcm-retrieval's already-folded context_summaries).
#   <name>.expect.sh  — run AFTER the root turn reaches a terminal status,
#     given session_key as $1 and root turn_id as $2; must exit 0 (pass) or
#     nonzero (fail), printing its own PASS/FAIL detail as it goes. A
#     scenario with no .expect.sh still runs (useful while iterating) but
#     is reported NO-ASSERTIONS, never silently counted as a pass — see
#     run_all.sh, which treats NO-ASSERTIONS as a failure for CI purposes.
set -euo pipefail

NAMESPACE=agents
PG_POD=abishekk-postgresql-0
PG_USER=agent_harness
PG_DB=agent_harness
TEMPORAL_TASK_QUEUE=agent-loop
TEMPORAL_NAMESPACE=abishekk

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
STARTER_BIN="$REPO_ROOT/.build/starter"

SCENARIO_NAME="${1:?usage: run_scenario.sh <scenario-name> [session-key]}"
SCENARIO_JSON="$SCRIPT_DIR/$SCENARIO_NAME.json"
SETUP_SQL="$SCRIPT_DIR/$SCENARIO_NAME.setup.sql"
EXPECT_SH="$SCRIPT_DIR/$SCENARIO_NAME.expect.sh"

if [ ! -f "$SCENARIO_JSON" ]; then
  echo "FAIL: no such scenario file: $SCENARIO_JSON" >&2
  exit 1
fi

RAND_SUFFIX="$(date +%s)-$$"
SESSION_KEY="${2:-test:scenario:$SCENARIO_NAME:$RAND_SUFFIX}"
ROOT_TURN_ID="${SESSION_KEY}:turn:1"

# --- Postgres helpers: every query runs server-side inside the postgres
# pod, password read from the pod's own secret file, never surfaced here.
pg_query() {
  kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -tAc \"$1\""
}
pg_exec_file() {
  # Applies a local .sql file via stdin — {{SESSION_KEY}} substituted first
  # so one setup file can seed data scoped to whatever session_key this run
  # picked, without the .sql file itself hardcoding one.
  sed "s/{{SESSION_KEY}}/${SESSION_KEY//\//\\/}/g" "$1" | \
    kubectl exec -i -n "$NAMESPACE" "$PG_POD" -- sh -c \
    "PGPASSWORD=\$(cat /opt/bitnami/postgresql/secrets/password) psql -U $PG_USER -d $PG_DB -v ON_ERROR_STOP=1"
}

echo "=== $SCENARIO_NAME ==="
echo "session_key: $SESSION_KEY"

if [ -f "$SETUP_SQL" ]; then
  echo "--- applying setup: $(basename "$SETUP_SQL") ---"
  pg_exec_file "$SETUP_SQL"
fi

echo "--- building starter (if needed) ---"
mkdir -p "$REPO_ROOT/.build"
if [ ! -x "$STARTER_BIN" ] || [ "$REPO_ROOT/workflows/cmd/starter/main.go" -nt "$STARTER_BIN" ]; then
  (cd "$REPO_ROOT/workflows" && go build -o "$STARTER_BIN" ./cmd/starter)
fi

# starter (a real Go binary, not kubectl exec) needs a direct TCP
# connection to Postgres, unlike every other Postgres interaction in this
# script — those all go through pg_query/pg_exec_file (kubectl exec, no
# local port-forward, password never local). Reuse an already-running
# port-forward on 15432 if one exists (e.g. run_all.sh's shared one across
# a whole suite run); otherwise start one just for this call and tear it
# down on exit — self-contained, no external setup required for a single
# `run_scenario.sh <name>` call to just work.
STARTED_PG_FORWARD=0
if ! nc -z localhost 15432 2>/dev/null; then
  kubectl port-forward -n "$NAMESPACE" svc/abishekk-postgresql 15432:5432 >/dev/null 2>&1 &
  PG_FORWARD_PID=$!
  STARTED_PG_FORWARD=1
  for _ in $(seq 1 20); do
    nc -z localhost 15432 2>/dev/null && break
    sleep 0.5
  done
  trap '[ "$STARTED_PG_FORWARD" = "1" ] && kill "$PG_FORWARD_PID" 2>/dev/null' EXIT
fi

echo "--- submitting scenario ---"
POSTGRES_PASSWORD="$(kubectl exec -n "$NAMESPACE" "$PG_POD" -- cat /opt/bitnami/postgresql/secrets/password)"
TEMPORAL_ADDRESS=localhost:17233 \
TEMPORAL_NAMESPACE="$TEMPORAL_NAMESPACE" \
TEMPORAL_TASK_QUEUE="$TEMPORAL_TASK_QUEUE" \
POSTGRES_HOST=localhost \
POSTGRES_PORT=15432 \
POSTGRES_USER="$PG_USER" \
POSTGRES_DB="$PG_DB" \
POSTGRES_PASSWORD="$POSTGRES_PASSWORD" \
  "$STARTER_BIN" -session "$SESSION_KEY" -scenario "$SCENARIO_JSON"

echo "--- waiting for root turn ($ROOT_TURN_ID) to reach a terminal status ---"
STATUS=""
# 180 iterations, not 60 — found live 2026-08-29: max-iterations.json alone
# needs ~80s of real activity time (20 iterations x ~4s each), before this
# loop's own per-poll kubectl-exec overhead (~1-2s/iteration) is even
# added — a 60s budget genuinely wasn't enough for a real multi-step
# scenario, not a hung workflow. 180 iterations gives real headroom without
# hiding an actually-hung workflow forever (still bounded, just generous).
for _ in $(seq 1 180); do
  STATUS="$(pg_query "SELECT status FROM turns WHERE turn_id = '$ROOT_TURN_ID'" || true)"
  case "$STATUS" in
    completed|failed|cancelled) break ;;
  esac
  sleep 1
done

if [ -z "$STATUS" ]; then
  echo "FAIL: $SCENARIO_NAME — root turn never appeared in Postgres (session=$SESSION_KEY)" >&2
  exit 1
fi
if [ "$STATUS" != "completed" ] && [ "$STATUS" != "failed" ] && [ "$STATUS" != "cancelled" ]; then
  echo "FAIL: $SCENARIO_NAME — root turn still '$STATUS' after the poll timeout (session=$SESSION_KEY)" >&2
  exit 1
fi
echo "root turn status: $STATUS"

if [ -f "$EXPECT_SH" ]; then
  echo "--- checking expectations: $(basename "$EXPECT_SH") ---"
  if PG_POD="$PG_POD" NAMESPACE="$NAMESPACE" PG_USER="$PG_USER" PG_DB="$PG_DB" \
     bash "$EXPECT_SH" "$SESSION_KEY" "$ROOT_TURN_ID"; then
    echo "PASS: $SCENARIO_NAME (session=$SESSION_KEY)"
  else
    echo "FAIL: $SCENARIO_NAME — expectations not met (session=$SESSION_KEY)" >&2
    exit 1
  fi
else
  echo "NO-ASSERTIONS: $SCENARIO_NAME ran to '$STATUS' but has no $SCENARIO_NAME.expect.sh (session=$SESSION_KEY)"
fi
