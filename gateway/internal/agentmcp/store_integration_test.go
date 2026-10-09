package agentmcp

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	wf "agent-harness/loop-worker/workflow"
	"agent-harness/shared/types"
)

type testTemporal struct {
	client.Client
	describe func(context.Context, string, string) (*workflowservice.DescribeWorkflowExecutionResponse, error)
	execute  func(context.Context, client.StartWorkflowOptions, interface{}, ...interface{}) (client.WorkflowRun, error)
	signal   func(context.Context, string, string, string, interface{}) error
}

func (c *testTemporal) DescribeWorkflowExecution(ctx context.Context, id, run string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
	return c.describe(ctx, id, run)
}

func (c *testTemporal) ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error) {
	return c.execute(ctx, opts, workflow, args...)
}

func (c *testTemporal) SignalWorkflow(ctx context.Context, id, run, name string, arg interface{}) error {
	return c.signal(ctx, id, run, name, arg)
}

func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("GATEWAY_MCP_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("set GATEWAY_MCP_TEST_POSTGRES_URL to a disposable PostgreSQL database")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "mcp_test_" + uuid.New().String()[:8]
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		admin.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, err := admin.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		if err != nil {
			t.Error(err)
		}
		admin.Close(ctx)
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = pool.Exec(ctx, `
		CREATE TABLE sessions (session_key text PRIMARY KEY, platform text NOT NULL, channel_id text NOT NULL);
		CREATE TABLE turns (turn_id text PRIMARY KEY, parent_id text NOT NULL, parent_type text NOT NULL,
		  status text NOT NULL, completed_at timestamptz);
		CREATE TABLE messages (parent_id text NOT NULL, role text NOT NULL, content text, seq int NOT NULL);
		CREATE TABLE tool_calls (tool_call_id text PRIMARY KEY, parent_id text NOT NULL, tool_name text NOT NULL,
		  status text NOT NULL, result jsonb, started_at timestamptz DEFAULT now(), completed_at timestamptz);
		CREATE TABLE user_input_requests (request_id text PRIMARY KEY, turn_id text NOT NULL, kind text NOT NULL,
		  prompt text NOT NULL, options jsonb NOT NULL, allow_free_text boolean NOT NULL, workflow_id text NOT NULL,
		  status text NOT NULL, created_at timestamptz DEFAULT now());`)
	if err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestDurableReplayAndNestedInputOwnership(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	executions := map[string]*workflowservice.DescribeWorkflowExecutionResponse{}
	starts, signals := 0, 0
	fake := &testTemporal{}
	fake.describe = func(_ context.Context, id, _ string) (*workflowservice.DescribeWorkflowExecutionResponse, error) {
		if d := executions[id]; d != nil {
			return d, nil
		}
		return nil, serviceerror.NewNotFound("missing workflow")
	}
	fake.execute = func(_ context.Context, opts client.StartWorkflowOptions, _ interface{}, args ...interface{}) (client.WorkflowRun, error) {
		starts++
		if opts.WorkflowIDReusePolicy != enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE || !opts.WorkflowExecutionErrorWhenAlreadyStarted {
			t.Fatal("workflow start can replay side effects")
		}
		in := args[0].(types.TurnInput)
		if in.TenantSlug != "tenant-one" || in.ParentType != "session" || in.PreInserted || in.InitialMessage.SpeakerID != "user_owner" {
			t.Fatalf("agent execution identity lost: %#v", in)
		}
		memo, err := converter.GetDefaultDataConverter().ToPayload(opts.Memo[queryMemoKey])
		if err != nil {
			t.Fatal(err)
		}
		executions[opts.ID] = &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflow.WorkflowExecutionInfo{
			Execution: &common.WorkflowExecution{WorkflowId: opts.ID, RunId: "run-1"},
			Status:    enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING,
			Memo:      &common.Memo{Fields: map[string]*common.Payload{queryMemoKey: memo}},
		}}
		exec("INSERT INTO turns (turn_id, parent_id, parent_type, status) VALUES ($1, $2, 'session', 'running')", in.TurnID, in.SessionKey)
		exec("INSERT INTO messages (parent_id, role, content, seq) VALUES ($1, 'user', $2, 0)", in.TurnID, in.InitialMessage.Content)
		exec("INSERT INTO turns (turn_id, parent_id, parent_type, status) VALUES ($1, $2, 'turn', 'running')", in.TurnID+":sub:1", in.TurnID)
		exec(`INSERT INTO user_input_requests (request_id, turn_id, kind, prompt, options, allow_free_text, workflow_id, status)
		      VALUES ($1, $2, 'permission', 'Allow this tool?', '[{"id":"approve","label":"Allow"},{"id":"deny","label":"Deny"}]', false, $3, 'pending')`,
			in.TurnID+":permission", in.TurnID+":sub:1", in.TurnID+":permission-workflow")
		return nil, nil
	}
	a := &agent{pool: pool, temporal: fake, tenantSlug: "tenant-one", taskQueue: "agent-loop"}
	in := input{RequestID: testRequestID, Query: "original query"}
	_, sessionKey, turnID := a.identity("user_owner", in.RequestID)
	fake.signal = func(_ context.Context, id, run, name string, arg interface{}) error {
		signals++
		if id != turnID+":permission-workflow" || name != wf.UserInputResponseSignalName {
			t.Fatalf("response escaped owned input: %s %s", id, name)
		}
		response := arg.(types.UserInputResponse)
		exec("UPDATE user_input_requests SET status = 'answered' WHERE request_id = $1", response.RequestID)
		exec("INSERT INTO messages (parent_id, role, content, seq) VALUES ($1, 'assistant', 'completed answer', 1)", turnID)
		exec("UPDATE turns SET status = 'completed', completed_at = now() WHERE turn_id = $1", turnID)
		exec(`INSERT INTO tool_calls (tool_call_id, parent_id, tool_name, status, result)
		      VALUES ($1, $2, 'wardrobe_context_get', 'ok', '{"version":9007199254740993}')`, turnID+":act:1", turnID)
		return nil
	}
	first, err := a.ask(ctx, "user_owner", in)
	if err != nil || first.Status != "needs_input" || first.PendingInput == nil || starts != 1 || signals != 0 {
		t.Fatalf("start/approval: %#v %v starts=%d signals=%d", first, err, starts, signals)
	}
	retry, err := a.ask(ctx, "user_owner", in)
	if err != nil || retry.TurnID != first.TurnID || starts != 1 {
		t.Fatalf("retry created a second turn: %#v %v starts=%d", retry, err, starts)
	}
	changed := in
	changed.Query = "different query"
	if _, err := a.ask(ctx, "user_owner", changed); err == nil || starts != 1 {
		t.Fatal("changed query replayed the request")
	}
	control := in
	control.Cancel = true
	if _, err := a.ask(ctx, "user_other", control); err == nil || starts != 1 || signals != 0 {
		t.Fatal("another owner cancelled or created this task")
	}
	control.Cancel = false
	control.Response = &inputResponse{RequestID: "foreign-input", SelectedOptionID: stringPointer("approve")}
	if _, err := a.ask(ctx, "user_owner", control); err == nil || signals != 0 {
		t.Fatal("a foreign input was answered")
	}
	control.Response.RequestID = first.PendingInput.RequestID
	control.Response.SelectedOptionID = stringPointer("unadvertised-option")
	if _, err := a.ask(ctx, "user_owner", control); err == nil || signals != 0 {
		t.Fatal("an unadvertised option reached Temporal")
	}
	control.Response.SelectedOptionID = stringPointer("approve")
	completed, err := a.ask(ctx, "user_owner", control)
	if err != nil || completed.Status != "completed" || signals != 1 {
		t.Fatalf("reviewed response did not complete: %#v %v", completed, err)
	}
	delete(executions, turnID)
	replayed, err := a.ask(ctx, "user_owner", control)
	if err != nil || replayed.Status != "completed" || starts != 1 || signals != 1 {
		t.Fatalf("completed replay after retention: %#v %v", replayed, err)
	}
	body, err := json.Marshal(replayed)
	if err != nil || len(replayed.Sources) != 1 || !strings.Contains(string(replayed.Sources[0].Result), "9007199254740993") {
		t.Fatalf("exact source JSON lost: %s %v", body, err)
	}
	exec("UPDATE turns SET status = 'running' WHERE turn_id = $1", turnID)
	_, err = a.ask(ctx, "user_owner", in)
	var public *publicError
	if !errors.As(err, &public) || public.code != "execution_unavailable" || starts != 1 {
		t.Fatalf("unavailable recorded turn was restarted: %v starts=%d", err, starts)
	}
	if _, found, err := a.readTurn(ctx, "user_other", sessionKey, turnID, in.Query); err != nil || found {
		t.Fatalf("SQL exposed another owner's turn: found=%v err=%v", found, err)
	}
}
