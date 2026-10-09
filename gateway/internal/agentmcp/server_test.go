package agentmcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"agent-harness/shared/types"
)

const testRequestID = "dc83c46d-62b2-43d3-8236-dabf2cd1d4d0"

func stringPointer(s string) *string { return &s }

func TestNormalizeInput(t *testing.T) {
	tests := []struct {
		name string
		in   input
		ok   bool
	}{
		{"query", input{RequestID: strings.ToUpper(testRequestID), Query: "  plan tomorrow  "}, true},
		{"uuid missing", input{Query: "query"}, false},
		{"uuid zero", input{RequestID: "00000000-0000-0000-0000-000000000000", Query: "query"}, false},
		{"empty query", input{RequestID: testRequestID, Query: "  "}, false},
		{"bytes not characters", input{RequestID: testRequestID, Query: strings.Repeat("👕", 1001)}, false},
		{"cancel", input{RequestID: testRequestID, Query: "query", Cancel: true}, true},
		{"cancel and answer", input{RequestID: testRequestID, Query: "query", Cancel: true, Response: &inputResponse{}}, false},
		{"option", input{RequestID: testRequestID, Query: "query", Response: &inputResponse{RequestID: "input-1", SelectedOptionID: stringPointer("approve")}}, true},
		{"both answers", input{RequestID: testRequestID, Query: "query", Response: &inputResponse{RequestID: "input-1", SelectedOptionID: stringPointer("approve"), FreeText: stringPointer("yes")}}, false},
		{"empty answer", input{RequestID: testRequestID, Query: "query", Response: &inputResponse{RequestID: "input-1", FreeText: stringPointer(" ")}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := normalize(&tt.in)
			if (err == nil) != tt.ok {
				t.Fatalf("normalize: %v", err)
			}
			if tt.ok && tt.in.RequestID != testRequestID {
				t.Fatalf("UUID was not normalized: %q", tt.in.RequestID)
			}
		})
	}
}

func TestResponseRequiresAdvertisedChoice(t *testing.T) {
	p := pendingInput{Options: []types.UserInputOption{{ID: "deny", Label: "Deny"}}}
	if validateResponse(&p, inputResponse{SelectedOptionID: stringPointer("approve")}) == nil {
		t.Fatal("unadvertised permission was accepted")
	}
	if validateResponse(&p, inputResponse{FreeText: stringPointer("approve")}) == nil {
		t.Fatal("free text bypassed the option restriction")
	}
	if err := validateResponse(&p, inputResponse{SelectedOptionID: stringPointer("deny")}); err != nil {
		t.Fatal(err)
	}
}

func TestResultBoundsPreserveJSONAndIntegerVersions(t *testing.T) {
	raw := json.RawMessage(`{"version":9007199254740993}`)
	body, err := encodeResult(result{Status: "completed", Sources: []source{{Result: raw}}})
	if err != nil || !strings.Contains(string(body), "9007199254740993") {
		t.Fatalf("integer version changed: %s (%v)", body, err)
	}
	body, err = encodeResult(result{Status: "completed", Answer: strings.Repeat("\x00", maxAnswerBytes)})
	if err != nil || len(body) > maxResultBytes || !json.Valid(body) || !strings.Contains(string(body), `"truncated":true`) {
		t.Fatalf("output limit failed: bytes=%d err=%v", len(body), err)
	}
	_, err = encodeResult(result{Status: "needs_input", PendingInput: &pendingInput{Prompt: strings.Repeat("x", maxResultBytes)}})
	if err == nil {
		t.Fatal("approval preview was silently truncated")
	}
	if text := clip("ab👕cd", 4); text != "ab" || !utf8.ValidString(text) {
		t.Fatalf("split UTF-8: %q", text)
	}
}

func TestToolErrorsHideInfrastructureDetails(t *testing.T) {
	out := toolError(&publicError{"request_conflict", "Use a different request_id"})
	if !out.IsError || !strings.Contains(out.Content[0].(*mcp.TextContent).Text, "request_conflict") {
		t.Fatalf("missing public error: %#v", out)
	}
	out = toolError(context.DeadlineExceeded)
	if !out.IsError || strings.Contains(out.Content[0].(*mcp.TextContent).Text, "deadline exceeded") {
		t.Fatal("internal failure leaked into the response")
	}
}

func TestHTTPAuthAndToolContract(t *testing.T) {
	verifier := func(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		if token != "owner-token" {
			return nil, auth.ErrInvalidToken
		}
		return &auth.TokenInfo{UserID: "user_owner", Expiration: time.Now().Add(time.Hour)}, nil
	}
	calls := 0
	handler := newHandler(verifier, func(ctx context.Context, owner string, in input) (result, error) {
		calls++
		if owner != "user_owner" || in.RequestID != testRequestID || in.Query != "plan tomorrow" {
			t.Fatalf("wrong trusted identity or arguments: %s %#v", owner, in)
		}
		return result{SchemaVersion: 1, RequestID: in.RequestID, Status: "completed", Answer: "Use the clean pieces."}, nil
	})
	request := func(token, origin, payload string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2025-06-18")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}
	call := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_agent","arguments":{"request_id":"` + testRequestID + `","query":"plan tomorrow"}}}`
	if r := request("wrong", "", call); r.Code != http.StatusUnauthorized || calls != 0 {
		t.Fatalf("unauthorized request reached agent: %d, calls=%d", r.Code, calls)
	}
	if r := request("owner-token", "https://untrusted.example", call); r.Code != http.StatusForbidden || calls != 0 {
		t.Fatalf("origin check failed: %d", r.Code)
	}
	if r := request("owner-token", "", call); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), "structuredContent") || calls != 1 {
		t.Fatalf("MCP call: %d %s, calls=%d", r.Code, r.Body.String(), calls)
	}
	unknownOwner := strings.Replace(call, `"query":"plan tomorrow"`, `"query":"plan tomorrow","user_id":"user_other"`, 1)
	if r := request("owner-token", "", unknownOwner); r.Code != http.StatusOK || !strings.Contains(r.Body.String(), `"isError":true`) || calls != 1 {
		t.Fatalf("owner injection reached agent: %d %s", r.Code, r.Body.String())
	}
	list := request("owner-token", "", `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if list.Code != http.StatusOK || !strings.Contains(list.Body.String(), `"name":"ask_agent"`) || !strings.Contains(list.Body.String(), "outputSchema") || strings.Contains(list.Body.String(), `"readOnlyHint":true`) {
		t.Fatalf("tool discovery/annotations: %d %s", list.Code, list.Body.String())
	}
}
