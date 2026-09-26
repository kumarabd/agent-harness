package registry_test

import (
	"agent-harness/workflows/internal/automation/connections"
	"agent-harness/workflows/internal/router/registry"
	"context"
	"encoding/json"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Uses an isolated schema and removes only that schema. Never points at a
// developer's tenant database implicitly; opt in with TEST_DATABASE_URL.
func database(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL for Postgres integration tests")
	}
	ctx := context.Background()
	root, err := pgxpool.New(ctx, dsn)
	require.NoError(t, err)
	schema := "connection_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = root.Exec(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	cfg, err := pgxpool.ParseConfig(dsn)
	require.NoError(t, err)
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		pool.Close()
		_, e := root.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
		require.NoError(t, e)
		root.Close()
	})
	for _, name := range []string{"001_tenant_registry.sql", "003_tenant_connections.sql", "004_connection_runtime.sql"} {
		b, e := os.ReadFile(filepath.Join("../../../../deploy/helm/agent-harness-shared/files", name))
		require.NoError(t, e)
		_, e = pool.Exec(ctx, string(b))
		require.NoError(t, e)
	}
	_, err = pool.Exec(ctx, `INSERT INTO tenant_registry(org_id,namespace,release_name) VALUES('org-a','tenant-a','a'),('org-b','tenant-b','b')`)
	require.NoError(t, err)
	return pool
}
func TestRequestConnectionTransactionAndIsolation(t *testing.T) {
	pool := database(t)
	r := registry.New(pool)
	ctx := context.Background()
	_, err := r.RequestConnection(ctx, "org-a", "github", "connected", "")
	require.ErrorIs(t, err, registry.ErrCredential)
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM tenant_connections").Scan(&n))
	require.Zero(t, n)
	c, err := r.RequestConnection(ctx, "org-a", "github", "connected", "private-token")
	require.NoError(t, err)
	_, err = r.RequestConnection(ctx, "org-a", "github", "disconnected", "")
	require.ErrorIs(t, err, registry.ErrConnectionBusy)
	_, err = r.RequestConnection(ctx, "org-a", "unknown", "connected", "")
	require.ErrorIs(t, err, registry.ErrIntegration)
	body, err := r.MarshalConnections(ctx, "org-a")
	require.NoError(t, err)
	require.NotContains(t, string(body), "private-token")
	other, err := r.ListConnections(ctx, "org-b")
	require.NoError(t, err)
	for _, item := range other {
		require.Nil(t, item.Connection)
	}
	var op string
	require.NoError(t, pool.QueryRow(ctx, "SELECT operation_id FROM tenant_connection_operations WHERE connection_id=$1", c.ID).Scan(&op))
	runtime := connections.Runtime{Pool: pool}
	require.NoError(t, runtime.Finish(ctx, op, true))
	retry, err := r.RequestConnection(ctx, "org-a", "github", "connected", "")
	require.NoError(t, err)
	require.Equal(t, c.ID, retry.ID)
	var rev int
	require.NoError(t, pool.QueryRow(ctx, "SELECT revision FROM tenant_connections WHERE connection_id=$1", c.ID).Scan(&rev))
	require.Equal(t, 2, rev)
	// A late repeated completion cannot overwrite a newer request.
	require.NoError(t, runtime.Finish(ctx, op, true))
	var state string
	require.NoError(t, pool.QueryRow(ctx, "SELECT state FROM tenant_connections WHERE connection_id=$1", c.ID).Scan(&state))
	require.Equal(t, "pending", state)
}
func TestConcurrentConnectDeduplicated(t *testing.T) {
	pool := database(t)
	r := registry.New(pool)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, e := r.RequestConnection(ctx, "org-a", "notion", "connected", "")
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	var n int
	require.NoError(t, pool.QueryRow(ctx, "SELECT count(*) FROM tenant_connection_operations").Scan(&n))
	require.Equal(t, 1, n)
}
func TestApplyBuildsOverridesAndRetryIsIdempotent(t *testing.T) {
	pool := database(t)
	r := registry.New(pool)
	ctx := context.Background()
	c, err := r.RequestConnection(ctx, "org-a", "github", "connected", "private-token")
	require.NoError(t, err)
	var op string
	require.NoError(t, pool.QueryRow(ctx, "SELECT operation_id FROM tenant_connection_operations WHERE connection_id=$1", c.ID).Scan(&op))
	upgrades := 0
	existing := []byte(`{"other":"retained"}`)
	runtime := connections.Runtime{Pool: pool, ChartDir: "/charts/tenant", PublicURL: "https://api.example.test", AllowedNamespaces: map[string]bool{"tenant-a": true}}
	runtime.Run = func(_ context.Context, input []byte, args ...string) ([]byte, error) {
		require.NotContains(t, strings.Join(args, " "), "private-token")
		switch strings.Join(args[:2], " ") {
		case "get metadata":
			return []byte(`{"chart":"tenant","version":"1","status":"deployed"}`), nil
		case "show chart":
			return []byte("name: tenant\nversion: '1'\n"), nil
		case "get values":
			return existing, nil
		case "upgrade a":
			upgrades++
			require.Contains(t, args, "--reuse-values")
			require.Contains(t, args, "--wait")
			var values map[string]any
			require.NoError(t, json.Unmarshal(input, &values))
			hub := values["mcp-hub"].(map[string]any)
			manifest := hub["manifests"].(map[string]any)["github.yaml"].(string)
			require.Contains(t, manifest, "private-token")
			require.Contains(t, manifest, c.ID+":1")
			require.NotContains(t, hub, "oauth") // unrelated backends keep their existing callback base
			existing = input
			return nil, nil
		default:
			t.Fatalf("unexpected helm args %v", args)
			return nil, nil
		}
	}
	require.NoError(t, runtime.Apply(ctx, op))
	require.NoError(t, runtime.Apply(ctx, op))
	require.Equal(t, 1, upgrades)
}
