package connections

import (
	"context"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestRenderManifest(t *testing.T) {
	raw := []byte(`{"transport":{"type":"http","url":"https://example.test/mcp"}}`)
	got, err := RenderManifest(raw, "github", "id:2", "connected", "header", "private-token", "")
	require.NoError(t, err)
	require.Contains(t, got, "connection_revision: id:2")
	require.Contains(t, got, "Authorization: Bearer private-token")
	_, err = RenderManifest(raw, "github", "id:2", "connected", "header", "", "")
	require.Error(t, err)
	_, err = RenderManifest(raw, "../../other", "id:2", "connected", "none", "", "")
	require.Error(t, err)
	got, err = RenderManifest(nil, "github", "id:3", "disconnected", "header", "", "")
	require.NoError(t, err)
	require.Nil(t, got)
	got, err = RenderManifest(raw, "notion", "id:1", "connected", "oauth", "", "https://router/hub/org/oauth/notion/callback")
	require.NoError(t, err)
	require.Contains(t, got, "redirect_uri: https://router/hub/org/oauth/notion/callback")
}
func TestContainsOverridePreservesUnrelatedValues(t *testing.T) {
	existing := []byte(`{"database":{"password":"retained"},"mcp-hub":{"manifests":{"manual.yaml":"manual","github.yaml":"new"},"oauth":{"mcpHubBaseUrl":"https://hub"}}}`)
	require.True(t, ContainsOverride(existing, []byte(`{"mcp-hub":{"manifests":{"github.yaml":"new","removed.yaml":null}}}`)))
	require.False(t, ContainsOverride(existing, []byte(`{"mcp-hub":{"manifests":{"github.yaml":null}}}`)))
	require.False(t, ContainsOverride(existing, []byte(`{"mcp-hub":{"manifests":{"github.yaml":"old"}}}`)))
}
func TestCheckChart(t *testing.T) {
	chart := []byte("name: tenant\nversion: 1.0.0\n")
	require.NoError(t, CheckChart([]byte(`{"chart":"tenant","version":"1.0.0","status":"deployed"}`), chart))
	require.Error(t, CheckChart([]byte(`{"chart":"tenant","version":"2.0.0","status":"deployed"}`), chart))
	require.Error(t, CheckChart([]byte(`{"chart":"tenant","version":"1.0.0","status":"pending-upgrade"}`), chart))
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestReadStatusRequiresCurrentRevision(t *testing.T) {
	target := Target{ConnectionID: "id", Revision: 2, Backend: "github", Desired: "connected", Release: "tenant", Namespace: "ns"}
	body := `{"items":[{"name":"github","revision":"id:1","state":"ready","tool_count":12}]}`
	a := Runtime{HTTP: &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "http://tenant-tools.ns.svc.cluster.local:8000/api/backends", r.URL.String())
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	state, count, err := a.ReadStatus(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, "indexing", state)
	require.Zero(t, count)
	body = strings.ReplaceAll(body, "id:1", "id:2")
	state, count, err = a.ReadStatus(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, "ready", state)
	require.Equal(t, 12, count)
	target.Desired = "disconnected"
	state, _, err = a.ReadStatus(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, "indexing", state)
	body = `{"items":[]}`
	state, _, err = a.ReadStatus(context.Background(), target)
	require.NoError(t, err)
	require.Equal(t, "disconnected", state)
}
