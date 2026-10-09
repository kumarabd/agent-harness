package agentmcp

import (
	"testing"

	"go.temporal.io/api/common/v1"
	"go.temporal.io/api/workflow/v1"
	"go.temporal.io/api/workflowservice/v1"
	"go.temporal.io/sdk/converter"
)

func TestRequestIdentityIncludesOwnerAndTenant(t *testing.T) {
	a := agent{tenantSlug: "tenant-one"}
	sid, key, turn := a.identity("user_one", testRequestID)
	if sid == "" || key == "" || turn != key+":turn:1" {
		t.Fatalf("inconsistent IDs: %s %s %s", sid, key, turn)
	}
	_, _, repeat := a.identity("user_one", testRequestID)
	_, _, otherOwner := a.identity("user_two", testRequestID)
	a.tenantSlug = "tenant-two"
	_, _, otherTenant := a.identity("user_one", testRequestID)
	if turn != repeat || turn == otherOwner || turn == otherTenant {
		t.Fatal("retry identity or tenant/owner isolation failed")
	}
}

func TestExecutionReplayChecksOriginalQuery(t *testing.T) {
	payload, err := converter.GetDefaultDataConverter().ToPayload(queryHash("original query"))
	if err != nil {
		t.Fatal(err)
	}
	description := &workflowservice.DescribeWorkflowExecutionResponse{WorkflowExecutionInfo: &workflow.WorkflowExecutionInfo{
		Memo: &common.Memo{Fields: map[string]*common.Payload{queryMemoKey: payload}},
	}}
	if err := checkExecution(description, "original query"); err != nil {
		t.Fatal(err)
	}
	if checkExecution(description, "changed query") == nil {
		t.Fatal("request UUID was reused for a different query")
	}
	if checkExecution(&workflowservice.DescribeWorkflowExecutionResponse{}, "original query") == nil {
		t.Fatal("a non-MCP execution was accepted")
	}
}
