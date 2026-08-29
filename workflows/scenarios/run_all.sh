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
# Deliberately excluded from SCENARIOS below, and why:
#   - real-llm-basic.json — needs a real provider API key and spends real
#     money; not part of the free regression suite. Run manually when
#     verifying real-provider integration specifically.
#   - interrupt-initial.json / interrupt-followup.json,
#     subagent-merge-cancelled-initial.json / -cancelled-followup.json —
#     each pair is two scripted scenarios chained against the SAME
#     still-running session (the second targets the first's active turn
#     as a follow-up). run_scenario.sh always mints a fresh session per
#     call, so these pairs aren't runnable through this simple one-shot
#     runner yet — a real, named gap, not silently dropped. Run manually
#     (two `run_scenario.sh <name> <same-session-key>` calls back to
#     back) until a chained-pair mode is added here.
#   - shell-exec-basic.json, shell-exec-parallel.json, multi-step-task.json,
#     subagent-merge-happy.json, subagent-merge-conflict.json — each
#     scripts a shell_exec call against a command (`echo`) that
#     permissions.py's real gating rules require approval for. That
#     dispatches a real UserInputRequestWorkflow which blocks for
#     `UserInputRequestTimeout` (1 hour) waiting for a response — and
#     nothing in this runner answers it (no gateway exists in this slice
#     to send one). Confirmed live 2026-08-29, not assumed: running the
#     full list once left four of these genuinely stuck for 10+ minutes
#     before being found and manually `temporal workflow terminate`-d — a
#     real, named gap in this runner's scope, not silently dropped from
#     the suite. Run manually and answer the approval yourself (`temporal
#     workflow signal --workflow-id <request_id> --name UserInputResponse
#     --input '{"request_id":"<request_id>","selected_option_id":"approve"}'`)
#     until auto-approval support is added here.
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

SCENARIOS=(
  happy-path
  shell-exec-slow
  max-iterations
  claim-check-large-output
  exploration-summary-json
  exploration-summary-csv
  exploration-summary-text
  subagent-spawn
  spawn-subagent-nested-valid
  spawn-subagent-nested-rejected
  lcm-retrieval
  lcm-grep-nested-fold
  anthropic-basic
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
