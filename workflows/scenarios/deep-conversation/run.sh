#!/usr/bin/env bash
# Deep Conversation Suite runner — see README.md for what each step validates.
#
# Runs entirely against local binaries pointed at the real cluster's Temporal/
# Postgres/mcp-hub/LiteLLM over kubectl port-forward, with the live cluster
# Deployments scaled to 0 so the local binaries are the only consumers (same
# methodology used throughout this project's live verification). Scales the
# cluster back to 1 replica each on exit, success or failure.
#
# Requires: kubectl context pointed at the right cluster, a Python venv at
# activities/.venv with the project installed, real credentials filled in
# below (do not commit real secrets into this file — copy the export block
# into a local, gitignored env file instead, same convention as this
# project's own scratchpad .env.test files).
set -euo pipefail

NAMESPACE=agents
TENANT_DEPLOYMENT=abishekk-agent-harness-tenant-tenant-worker
SHARED_DEPLOYMENT=harness-agent-harness-shared
POSTGRES_SVC=abishekk-postgresql
MCP_HUB_SVC=abishekk
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
SCENARIOS_DIR="$REPO_ROOT/workflows/scenarios"
SUITE_DIR="$SCENARIOS_DIR/deep-conversation"

: "${PIONEER_API_KEY:?set PIONEER_API_KEY before running}"
: "${AGENT_BRAIN_API_KEY:?set AGENT_BRAIN_API_KEY before running}"
: "${EMBEDDING_API_KEY:?set EMBEDDING_API_KEY before running}"

PIONEER_BASE_URL_REAL="https://api.inference.crusoecloud.com/v1"
PIONEER_MODEL="deepseek-ai/DeepSeek-V4-Pro"

cleanup() {
  echo "--- cleanup ---"
  pkill -f "$REPO_ROOT/.build/loop-worker" 2>/dev/null || true
  pkill -f "activities.tenant_worker" 2>/dev/null || true
  pkill -f "port-forward.*temporal-frontend" 2>/dev/null || true
  pkill -f "port-forward.*$POSTGRES_SVC" 2>/dev/null || true
  pkill -f "port-forward.*litellm-service" 2>/dev/null || true
  pkill -f "port-forward.*svc/$MCP_HUB_SVC " 2>/dev/null || true
  kubectl scale deployment -n "$NAMESPACE" "$TENANT_DEPLOYMENT" --replicas=1 || true
  kubectl scale deployment -n "$NAMESPACE" "$SHARED_DEPLOYMENT" --replicas=1 || true
}
trap cleanup EXIT

echo "--- building local binaries ---"
mkdir -p "$REPO_ROOT/.build"
(cd "$REPO_ROOT/workflows" && go build -o "$REPO_ROOT/.build/loop-worker" ./cmd/loop-worker)
(cd "$REPO_ROOT/workflows" && go build -o "$REPO_ROOT/.build/starter" ./cmd/starter)

echo "--- port-forwards ---"
kubectl port-forward -n core svc/temporal-frontend 7233:7233 > /tmp/dc-pf-temporal.log 2>&1 &
kubectl port-forward -n "$NAMESPACE" "svc/$POSTGRES_SVC" 5433:5432 > /tmp/dc-pf-postgres.log 2>&1 &
kubectl port-forward -n core svc/litellm-service 4000:4000 > /tmp/dc-pf-litellm.log 2>&1 &
kubectl port-forward -n "$NAMESPACE" "svc/$MCP_HUB_SVC" 8000:8000 > /tmp/dc-pf-mcphub.log 2>&1 &
sleep 3
nc -zv localhost 7233
nc -zv localhost 5433
nc -zv localhost 4000
nc -zv localhost 8000

echo "--- scaling live cluster deployments to 0 ---"
kubectl scale deployment -n "$NAMESPACE" "$TENANT_DEPLOYMENT" --replicas=0
kubectl scale deployment -n "$NAMESPACE" "$SHARED_DEPLOYMENT" --replicas=0
sleep 5

COMMON_ENV=(
  TEMPORAL_ADDRESS=localhost:7233
  TEMPORAL_NAMESPACE=abishekk
  TEMPORAL_TASK_QUEUE=agent-loop
  POSTGRES_HOST=localhost
  POSTGRES_PORT=5433
  POSTGRES_USER=agent_harness
  POSTGRES_PASSWORD="$POSTGRES_PASSWORD"
  POSTGRES_DB=agent_harness
  AGENT_BRAIN_BASE_URL=http://localhost:8080
  AGENT_BRAIN_API_KEY="$AGENT_BRAIN_API_KEY"
  AGENT_BRAIN_AGENT_ID=agent-harness
  PIONEER_MODEL="$PIONEER_MODEL"
  PIONEER_API_KEY="$PIONEER_API_KEY"
  MCP_HUB_URL=http://localhost:8000
  EMBEDDING_BASE_URL=http://localhost:4000
  EMBEDDING_MODEL=text-embedding-3-small
  EMBEDDING_DIM=768
  EMBEDDING_API_KEY="$EMBEDDING_API_KEY"
)

start_loop_worker() {
  env "${COMMON_ENV[@]}" "$REPO_ROOT/.build/loop-worker" > /tmp/dc-loop-worker.log 2>&1 &
  echo $!
}

start_tenant_worker() {
  local base_url="$1"
  (cd "$REPO_ROOT/activities" && source .venv/bin/activate && \
    env "${COMMON_ENV[@]}" PIONEER_BASE_URL="$base_url" "$@" \
    python3 -m activities.tenant_worker) > /tmp/dc-tenant-worker.log 2>&1 &
  echo $!
}

run_scenario() {
  local session="$1" file="$2"
  env "${COMMON_ENV[@]}" "$REPO_ROOT/.build/starter" -session "$session" -scenario "$file"
}

echo "=== Phase 1: main chained session (deep-conv-1) ==="
LOOP_PID=$(start_loop_worker)
TENANT_PID=$(env "${COMMON_ENV[@]}" LANGUAGE_MEDIUM_CONTEXT_WINDOW=8000 \
  bash -c "cd '$REPO_ROOT/activities' && source .venv/bin/activate && env PIONEER_BASE_URL='$PIONEER_BASE_URL_REAL' python3 -m activities.tenant_worker > /tmp/dc-tenant-worker.log 2>&1 & echo \$!")
sleep 3

SESSION=deep-conv-1
run_scenario "$SESSION" "$SCENARIOS_DIR/shell-exec-basic.json"; sleep 5
run_scenario "$SESSION" "$SCENARIOS_DIR/shell-exec-parallel.json"; sleep 5
run_scenario "$SESSION" "$SCENARIOS_DIR/subagent-spawn.json"; sleep 5
run_scenario "$SESSION" "$SUITE_DIR/04-remember-fact.json"; sleep 5

echo "--- sleeping 35s past idleTTL before step 5, to force a fresh coordinator ---"
sleep 35
run_scenario "$SESSION" "$SUITE_DIR/05-recall-fact.json"; sleep 5
run_scenario "$SESSION" "$SUITE_DIR/06-tool-search.json"; sleep 5

for f in "$SUITE_DIR"/07*-compress-*.json; do
  run_scenario "$SESSION" "$f"
  sleep 5
done
run_scenario "$SESSION" "$SUITE_DIR/08-post-compress-check.json"; sleep 5

kill "$TENANT_PID" 2>/dev/null || true
pkill -f "activities.tenant_worker" 2>/dev/null || true
sleep 2

echo "=== Phase 2: isolated scenarios ==="
TENANT_PID=$(start_tenant_worker "$PIONEER_BASE_URL_REAL")
sleep 3

run_scenario interrupt-demo "$SCENARIOS_DIR/interrupt-initial.json"
sleep 2
run_scenario interrupt-demo "$SCENARIOS_DIR/interrupt-followup.json"
sleep 5

run_scenario max-iter-demo "$SCENARIOS_DIR/max-iterations.json"
sleep 5

run_scenario tool-retry-demo "$SUITE_DIR/isolated/tool-call-retry.json"
sleep 5

kill "$TENANT_PID" 2>/dev/null || true
pkill -f "activities.tenant_worker" 2>/dev/null || true
sleep 2

echo "=== Phase 3: induced failure (broken endpoint) ==="
TENANT_PID=$(start_tenant_worker "http://127.0.0.1:1")
sleep 3
run_scenario induced-failure-demo "$SUITE_DIR/isolated/induced-failure.json"
sleep 10
kill "$TENANT_PID" 2>/dev/null || true

kill "$LOOP_PID" 2>/dev/null || true

echo "=== Done. Verify with: ==="
cat <<'EOF'
psql (via port-forward on 5433) — check:
  SELECT turn_id, turn_seq, status FROM turns WHERE parent_id IN
    ('deep-conv-1','interrupt-demo','max-iter-demo','tool-retry-demo','induced-failure-demo')
    ORDER BY parent_id, turn_seq;
  SELECT kind, token_count, created_at FROM context_summaries
    WHERE session_key = 'deep-conv-1' ORDER BY created_at;
  SELECT tool_name, status FROM tool_calls WHERE parent_id LIKE 'tool-retry-demo%';
temporal workflow list --query "WorkflowId='deep-conv-1'"
EOF
