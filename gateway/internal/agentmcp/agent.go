package agentmcp

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/converter"

	"agent-harness/gateway/internal/core"
	wf "agent-harness/loop-worker/workflow"
	"agent-harness/shared/ids"
	"agent-harness/shared/types"
)

const queryMemoKey = "mcp_query_sha256"

type agent struct {
	pool       *pgxpool.Pool
	temporal   client.Client
	taskQueue  string
	tenantSlug string
}

func (a *agent) identity(owner, requestID string) (string, string, string) {
	// Tenant identity matters because tenants share a Temporal namespace.
	sessionID := "mcp-" + a.tenantSlug + "-" + requestID
	sessionKey := core.SessionKeyFor("web", owner, "session:"+sessionID)
	return sessionID, sessionKey, ids.TurnID(sessionKey, 1)
}

func queryHash(query string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(query))) }

func checkExecution(description *workflowservice.DescribeWorkflowExecutionResponse, query string) error {
	payload := description.GetWorkflowExecutionInfo().GetMemo().GetFields()[queryMemoKey]
	var hash string
	if payload == nil || converter.GetDefaultDataConverter().FromPayload(payload, &hash) != nil || hash != queryHash(query) {
		return &publicError{"request_conflict", "request_id already belongs to a different query or execution"}
	}
	return nil
}

func (a *agent) ask(ctx context.Context, owner string, in input) (result, error) {
	sessionID, sessionKey, turnID := a.identity(owner, in.RequestID)
	out, found, err := a.readTurn(ctx, owner, sessionKey, turnID, in.Query)
	if err != nil {
		return result{}, err
	}
	finish := func(out result) result {
		out.SchemaVersion, out.RequestID, out.SessionID, out.TurnID = 1, in.RequestID, sessionID, turnID
		out.RetrievedAt = time.Now().UTC()
		out.Coverage = "recent_top_level_tool_calls"
		if out.Status == "running" || out.Status == "cancelling" {
			out.RetryAfterMS = 1000
		}
		return out
	}
	if terminal(out.Status) {
		return finish(out), nil
	}

	description, err := a.temporal.DescribeWorkflowExecution(ctx, turnID, "")
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		// Never restart a recorded turn after Temporal history has expired.
		if found {
			return result{}, &publicError{"execution_unavailable", "Recorded execution is unavailable; this request will not be restarted"}
		}
		if in.Cancel || in.Response != nil {
			return result{}, &publicError{"not_found", "No agent request exists for this request_id"}
		}
		if _, err := a.pool.Exec(ctx,
			"INSERT INTO sessions (session_key, platform, channel_id) VALUES ($1, 'web', $2) ON CONFLICT (session_key) DO NOTHING",
			sessionKey, owner,
		); err != nil {
			return result{}, err
		}
		seq := 1
		_, err = a.temporal.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
			ID: turnID, TaskQueue: a.taskQueue, WorkflowRunTimeout: 30 * time.Minute,
			WorkflowIDReusePolicy:                    enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE,
			WorkflowExecutionErrorWhenAlreadyStarted: true,
			Memo:                                     map[string]any{queryMemoKey: queryHash(in.Query)},
		}, wf.TurnWorkflow, types.TurnInput{
			SessionKey: sessionKey, TurnID: turnID, TenantSlug: a.tenantSlug,
			ParentType: "session", ParentID: sessionKey, TurnSeq: &seq,
			InitialMessage: types.Message{Role: "user", Content: in.Query, SpeakerID: owner, ClientMsgID: in.RequestID},
		})
		var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
		if err != nil && !errors.As(err, &duplicate) {
			return result{}, err
		}
		description, err = a.temporal.DescribeWorkflowExecution(ctx, turnID, "")
	}
	if err != nil {
		return result{}, err
	}
	if err := checkExecution(description, in.Query); err != nil {
		return result{}, err
	}
	execution := description.GetWorkflowExecutionInfo()
	if execution.GetStatus() != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
		out, _, err = a.readTurn(ctx, owner, sessionKey, turnID, in.Query)
		if err != nil {
			return result{}, err
		}
		if !terminal(out.Status) {
			out.Status = "failed"
			if execution.GetStatus() == enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED {
				out.Status = "cancelled"
			}
			if execution.GetCloseTime() != nil {
				closed := execution.GetCloseTime().AsTime()
				out.CompletedAt = &closed
			}
		}
		return finish(out), nil
	}
	runID := execution.GetExecution().GetRunId()
	if in.Cancel {
		// Cooperative cancellation also closes parked approvals and persists the turn status.
		if err := a.temporal.SignalWorkflow(ctx, turnID, runID, wf.CancelSignalName, struct{}{}); err != nil {
			return result{}, err
		}
		out.Status = "cancelling"
		return finish(out), nil
	}
	if in.Response != nil {
		if err := a.respond(ctx, turnID, *in.Response); err != nil {
			return result{}, err
		}
	}

	deadline := time.NewTimer(resultWait)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		out, _, err = a.readTurn(ctx, owner, sessionKey, turnID, in.Query)
		if err != nil {
			return result{}, err
		}
		if terminal(out.Status) {
			return finish(out), nil
		}
		pending, err := a.readInput(ctx, turnID, "")
		if err != nil {
			return result{}, err
		}
		if pending != nil {
			out.Status, out.PendingInput = "needs_input", &pending.pendingInput
			return finish(out), nil
		}
		select {
		case <-ctx.Done():
			return result{}, ctx.Err()
		case <-deadline.C:
			return finish(out), nil
		case <-ticker.C:
		}
	}
}

func terminal(status string) bool {
	return status == "completed" || status == "failed" || status == "cancelled"
}

func validateResponse(p *pendingInput, response inputResponse) error {
	if response.FreeText != nil && p.AllowFreeText {
		return nil
	}
	if response.SelectedOptionID != nil {
		for _, option := range p.Options {
			if option.ID == *response.SelectedOptionID {
				return nil
			}
		}
	}
	return invalid("Response is not permitted by this pending input")
}

func (a *agent) respond(ctx context.Context, turnID string, response inputResponse) error {
	p, err := a.readInput(ctx, turnID, response.RequestID)
	if err != nil {
		return err
	}
	if p == nil {
		return &publicError{"not_found", "Input request was not found in this agent request"}
	}
	if p.status != "pending" {
		return nil
	}
	if err := validateResponse(&p.pendingInput, response); err != nil {
		return err
	}
	return a.temporal.SignalWorkflow(ctx, p.workflowID, "", wf.UserInputResponseSignalName, types.UserInputResponse{
		RequestID: response.RequestID, SelectedOptionID: response.SelectedOptionID, FreeText: response.FreeText,
	})
}
