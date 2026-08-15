// Command starter is the local stand-in for the Gateway's inbound path
// (components/gateway.md): it does the one thing the gateway does after dedup —
// SignalWithStart against the Session Coordinator (workflow ID = session key),
// exactly as 02-architecture-temporal-execution.md §3 describes.
//
// Under the reference-passing contract (docs/components/temporal-workflow.md,
// "Resolved: Reference/ID Schema"), scripted_model_responses is test-fixture
// content and can't be forwarded through the workflow — this command writes
// it directly to Postgres's _test_scripted_responses table, keyed by the
// turn_id the coordinator is about to create, before signaling.
//
// Usage:
//
//	go run ./cmd/starter -session <session_key> -scenario <path/to/scenario.json>
//
// A scenario file is: {"message": {...}, "scripted_model_responses": [...]}.
// See workflows/scenarios/.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/client"

	"agent-harness/workflows/internal/ids"
	"agent-harness/workflows/internal/types"
	wf "agent-harness/workflows/internal/workflow"
)

// scenarioModelResponse mirrors the JSON shape of one scripted response in a
// scenario file — kept local to the starter since it's fixture-authoring
// shape, not a type the workflow or a real activity ever sees.
type scenarioModelResponse struct {
	Content   string             `json:"content"`
	ToolCalls []scenarioToolCall `json:"tool_calls"`
	Usage     types.Usage        `json:"usage"`
}

type scenarioToolCall struct {
	Name       string         `json:"name"`
	IsSubagent bool           `json:"is_subagent"`
	Arguments  map[string]any `json:"arguments"`
}

type scenario struct {
	Message                types.Message           `json:"message"`
	ScriptedModelResponses []scenarioModelResponse `json:"scripted_model_responses"`
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	sessionKey := flag.String("session", "", "session key (Session Coordinator workflow ID)")
	scenarioPath := flag.String("scenario", "", "path to a scenario JSON file")
	flag.Parse()

	if *sessionKey == "" || *scenarioPath == "" {
		log.Fatal("both -session and -scenario are required")
	}

	raw, err := os.ReadFile(*scenarioPath)
	if err != nil {
		log.Fatalf("failed to read scenario file: %v", err)
	}
	var sc scenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		log.Fatalf("failed to parse scenario JSON: %v", err)
	}

	ctx := context.Background()

	pgURL := fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s",
		envOrDefault("POSTGRES_USER", "agent_harness"),
		envOrDefault("POSTGRES_PASSWORD", ""),
		envOrDefault("POSTGRES_HOST", "localhost"),
		envOrDefault("POSTGRES_PORT", "5432"),
		envOrDefault("POSTGRES_DB", "agent_harness"),
	)
	pool, err := pgxpool.New(ctx, pgURL)
	if err != nil {
		log.Fatalf("unable to connect to Postgres: %v", err)
	}
	defer pool.Close()

	// If this session already has an active (still-running) top-level turn,
	// the coordinator forwards a new signal into THAT turn rather than
	// starting a fresh one (02-architecture-temporal-execution.md §2, the
	// active-session guard) — this is exactly what the
	// interrupt-initial/interrupt-followup scenario pair (README.md) relies
	// on: run interrupt-initial, then run interrupt-followup against the
	// same -session while it's still in flight, and the follow-up should
	// fold into the SAME turn, not start a second one. So the starter has to
	// detect that case and target the running turn's ID, continuing its
	// fixture sequence, instead of always minting a new turn_id.
	var turnID string
	var startSeq int
	var runningTurnID string
	err = pool.QueryRow(ctx,
		"SELECT turn_id FROM turns WHERE parent_id = $1 AND parent_type = 'session' AND status = 'running' "+
			"ORDER BY turn_seq DESC LIMIT 1",
		*sessionKey,
	).Scan(&runningTurnID)
	switch {
	case err == nil:
		turnID = runningTurnID
		var maxFixtureSeq *int
		if err := pool.QueryRow(ctx,
			"SELECT MAX(seq) FROM _test_scripted_responses WHERE turn_id = $1", turnID,
		).Scan(&maxFixtureSeq); err != nil {
			log.Fatalf("failed to query current fixture seq for active turn %q: %v", turnID, err)
		}
		if maxFixtureSeq != nil {
			startSeq = *maxFixtureSeq + 1
		}
		log.Printf("session %q has an active turn %q — targeting it as a follow-up (fixture seq starts at %d)",
			*sessionKey, turnID, startSeq)
	case errors.Is(err, pgx.ErrNoRows):
		// The coordinator computes turn_id the same way — MAX(turn_seq)+1 for
		// this session (components/session-coordinator.md) — so the starter
		// replicates that exact query to predict the turn_id it's about to
		// cause the coordinator to create. This is inherently a race if two
		// starters ever ran concurrently against the same session; fine for
		// this single-writer local-dev tool, not fine as a general mechanism
		// (real inbound writes never mint turn_seq client-side — only the
		// coordinator does).
		var maxSeq *int
		if err := pool.QueryRow(ctx,
			"SELECT MAX(turn_seq) FROM turns WHERE parent_id = $1 AND parent_type = 'session'",
			*sessionKey,
		).Scan(&maxSeq); err != nil {
			log.Fatalf("failed to query current turn_seq: %v", err)
		}
		nextSeq := 1
		if maxSeq != nil {
			nextSeq = *maxSeq + 1
		}
		turnID = ids.TurnID(*sessionKey, nextSeq)
	default:
		log.Fatalf("failed to query for an active turn: %v", err)
	}

	if err := writeFixtures(ctx, pool, turnID, startSeq, sc.ScriptedModelResponses); err != nil {
		log.Fatalf("failed to write test fixtures: %v", err)
	}

	c, err := client.Dial(client.Options{
		HostPort:  envOrDefault("TEMPORAL_ADDRESS", client.DefaultHostPort),
		Namespace: envOrDefault("TEMPORAL_NAMESPACE", client.DefaultNamespace),
	})
	if err != nil {
		log.Fatalf("unable to create Temporal client: %v", err)
	}
	defer c.Close()

	// AllowDuplicate: the common case is a clean coordinator TTL exit, and the
	// uncommon case is a crash — both need the next SignalWithStart to
	// succeed (02-architecture-temporal-execution.md §2, "Reuse and crash behavior").
	opts := client.StartWorkflowOptions{
		ID:                    *sessionKey,
		TaskQueue:             envOrDefault("TEMPORAL_TASK_QUEUE", "agent-loop"),
		WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
	}

	payload := types.SignalPayload{Message: sc.Message}

	we, err := c.SignalWithStartWorkflow(
		ctx,
		*sessionKey,
		wf.NewMessageSignalName,
		payload,
		opts,
		wf.CoordinatorWorkflow,
		wf.CoordinatorInput{SessionKey: *sessionKey},
	)
	if err != nil {
		log.Fatalf("SignalWithStart failed: %v", err)
	}

	log.Printf("signalled session %q — coordinator run ID %s (workflow ID %s), expecting turn_id %q",
		*sessionKey, we.GetRunID(), we.GetID(), turnID)
}

// writeFixtures recursively writes scripted responses for turnID, starting at
// fixture seq startSeq (nonzero when turnID is an already-active turn being
// extended by a follow-up — see the active-turn detection in main above), and
// for any subagent tool call within them, precomputes that subagent's own
// turn_id using the *same* deterministic minting scheme ModelCall itself uses
// (activities/activities/model_call.py: n is cumulative across the WHOLE
// turn — offset by however many tool_calls rows already exist for turnID —
// not reset per response, since tool_call_id is a flat per-turn-unique
// Postgres primary key and a turn's loop can call ModelCall many times) and
// recurses into its own scripted_model_responses (always starting a fresh
// subagent turn at seq 0 and offset 0 — a subagent turn is always new).
func writeFixtures(ctx context.Context, pool *pgxpool.Pool, turnID string, startSeq int, responses []scenarioModelResponse) error {
	var nOffset int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM tool_calls WHERE parent_id = $1", turnID,
	).Scan(&nOffset); err != nil {
		return fmt.Errorf("counting existing tool_calls for turn %q: %w", turnID, err)
	}

	for i, resp := range responses {
		seq := startSeq + i
		toolCallsJSON, err := json.Marshal(resp.ToolCalls)
		if err != nil {
			return err
		}
		usageJSON, err := json.Marshal(resp.Usage)
		if err != nil {
			return err
		}
		_, err = pool.Exec(ctx,
			"INSERT INTO _test_scripted_responses (turn_id, seq, content, tool_calls, usage) VALUES ($1, $2, $3, $4, $5)",
			turnID, seq, resp.Content, toolCallsJSON, usageJSON,
		)
		if err != nil {
			return fmt.Errorf("writing fixture for turn %q seq %d: %w", turnID, seq, err)
		}

		for _, tc := range resp.ToolCalls {
			nOffset++
			if !tc.IsSubagent {
				continue
			}
			subTurnID := ids.SubagentTurnID(turnID, nOffset) // matches model_call.py's turn-cumulative counter
			nested, ok := tc.Arguments["scripted_model_responses"]
			if !ok {
				continue
			}
			nestedJSON, err := json.Marshal(nested)
			if err != nil {
				return err
			}
			var nestedResponses []scenarioModelResponse
			if err := json.Unmarshal(nestedJSON, &nestedResponses); err != nil {
				return fmt.Errorf("parsing nested scripted_model_responses for subagent %q: %w", subTurnID, err)
			}
			if err := writeFixtures(ctx, pool, subTurnID, 0, nestedResponses); err != nil {
				return err
			}
		}
	}
	return nil
}
