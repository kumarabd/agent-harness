#!/usr/bin/env bash
# The regression suite entry point — runs every standalone, zero-cost,
# scripted scenario via run_scenario.sh and reports a final PASS/FAIL
# summary. This is the thing to run after any change touching ModelCall,
# turn.go's dispatch loop, tool_calls minting, or the lcm/ package — no
# live cluster scaling, no local worker binaries, no real API spend.
#
# Usage: workflows/scenarios/run_all.sh
# (same prerequisites as run_scenario.sh — see its own header)
#
# CHAINED_PAIRS below run a <name>-initial / <name>-followup pair against ONE
# session: -initial starts a turn, then (while it's still in flight) -followup
# signals the same session so the coordinator folds it into the running turn.
# Only <name>-followup.expect.sh is checked.
#
# Gated shell_exec calls (`echo` needs approval per permissions.py) are handled
# by run_scenario.sh's built-in auto-approver — it polls for the pending
# permission request and signals it 'approve'. No manual step any more.
#
# Deliberately still excluded:
#   - real-llm-basic.json / real-llm-pipeline-* — need a real provider API key
#     and spend real money.
#   - subagent-merge-happy / -conflict / -cancelled-* — their
#     merge_subagent_output arg hardcodes a fixed session key
#     ("merge-happy:turn:1:sub:1" etc.), so they only run under that exact
#     -session. Run manually:
#       run_scenario.sh subagent-merge-happy merge-happy
#   - multi-step-task.json, shell-exec-parallel.json — no .expect.sh yet.
#
# Add a new scenario to this suite by: (1) dropping <name>.json (and,
# ideally, <name>.expect.sh — see run_scenario.sh's own header) into this
# directory, (2) adding its name to SCENARIOS below. That's the whole
# process — no other registration needed.
set -uo pipefail

NAMESPACE=agents
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# One shared port-forward pair for the whole suite run, rather than each
# run_scenario.sh call starting/tearing down its own 17 times — each
# individual call still self-manages if run standalone (see its own
# header), this just avoids the churn when running the full suite.
cleanup_forwards() {
  [ -n "${TEMPORAL_FWD_PID:-}" ] && kill "$TEMPORAL_FWD_PID" 2>/dev/null
  [ -n "${PG_FWD_PID:-}" ] && kill "$PG_FWD_PID" 2>/dev/null
}
trap cleanup_forwards EXIT

if ! nc -z localhost 17233 2>/dev/null; then
  kubectl port-forward -n core svc/temporal-frontend 17233:7233 >/dev/null 2>&1 &
  TEMPORAL_FWD_PID=$!
fi
if ! nc -z localhost 15432 2>/dev/null; then
  kubectl port-forward -n "$NAMESPACE" svc/abishekk-postgresql 15432:5432 >/dev/null 2>&1 &
  PG_FWD_PID=$!
fi
for _ in $(seq 1 20); do
  nc -z localhost 17233 2>/dev/null && nc -z localhost 15432 2>/dev/null && break
  sleep 0.5
done

# Clear any leftover test:% data from a prior run BEFORE starting — several
# setup.sql files seed fixed-UUID rows (lcm-retrieval, lcm-grep-nested-fold,
# real-assembly) that collide on messages_pkey if a previous run's rows are
# still around. The setup files also self-clean their own UUIDs, but this
# also sweeps stale turns/sessions/skill_procedures the suite created.
echo "--- cleaning leftover test data ---"
bash "$SCRIPT_DIR/cleanup_test_data.sh" || echo "  (cleanup had non-fatal errors — continuing)"

SCENARIOS=(
  happy-path
  shell-exec-slow
  shell-exec-basic
  max-iterations
  claim-check-large-output
  exploration-summary-json
  exploration-summary-csv
  exploration-summary-text
  lcm-retrieval
  lcm-grep-nested-fold
  anthropic-basic
  lite-simple-task
  subagent-spawn
  spawn-subagent-nested-valid
  spawn-subagent-nested-rejected
  subagent-full-agent
  parallel-subagents
  real-assembly
  resolved-tool-dispatch
  blocked-terminal
  no-progress-guard
  ceiling-raise
)

# <name>-initial started, then <name>-followup signalled into the same
# still-running session. Only <name>-followup.expect.sh is checked.
CHAINED_PAIRS=(
  interrupt
  ask-user
)

PASSED=()
FAILED=()
NO_ASSERTIONS=()

for name in "${SCENARIOS[@]}"; do
  echo ""
  output="$(bash "$SCRIPT_DIR/run_scenario.sh" "$name" 2>&1)"
  status=$?
  echo "$output"
  if [ $status -ne 0 ]; then
    FAILED+=("$name")
  elif echo "$output" | grep -q "^NO-ASSERTIONS:"; then
    NO_ASSERTIONS+=("$name")
  else
    PASSED+=("$name")
  fi
done

for pair in "${CHAINED_PAIRS[@]}"; do
  echo ""
  echo "=== chained pair: $pair ==="
  key="test:scenario:${pair}:$(date +%s)-$RANDOM"
  # -initial in the background: it starts the turn and waits for it to finish,
  # which is fine — the followup below folds into that same still-running turn.
  bash "$SCRIPT_DIR/run_scenario.sh" "${pair}-initial" "$key" >/dev/null 2>&1 &
  init_pid=$!
  sleep 3
  output="$(bash "$SCRIPT_DIR/run_scenario.sh" "${pair}-followup" "$key" 2>&1)"
  status=$?
  wait "$init_pid" 2>/dev/null || true
  echo "$output"
  if [ $status -ne 0 ]; then
    FAILED+=("${pair}-pair")
  elif echo "$output" | grep -q "^NO-ASSERTIONS:"; then
    NO_ASSERTIONS+=("${pair}-pair")
  else
    PASSED+=("${pair}-pair")
  fi
done

echo ""
echo "=================================================="
echo "PASSED (${#PASSED[@]}): ${PASSED[*]:-none}"
echo "NO-ASSERTIONS (${#NO_ASSERTIONS[@]}): ${NO_ASSERTIONS[*]:-none}"
echo "FAILED (${#FAILED[@]}): ${FAILED[*]:-none}"
echo "=================================================="

# NO-ASSERTIONS counts as a failure for suite purposes — a scenario that
# only ran without checking anything isn't real regression coverage, same
# reasoning run_scenario.sh's own header states.
if [ ${#FAILED[@]} -gt 0 ] || [ ${#NO_ASSERTIONS[@]} -gt 0 ]; then
  exit 1
fi
exit 0
