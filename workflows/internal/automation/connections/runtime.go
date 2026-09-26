// Package connections implements connection provisioning activities and the
// transactional outbox dispatcher. Only operation IDs enter Temporal history.
package connections

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.temporal.io/sdk/activity"
	"golang.org/x/sync/errgroup"
	"gopkg.in/yaml.v3"
)

type Runtime struct {
	Pool              *pgxpool.Pool
	ChartDir          string
	PublicURL         string
	AllowedNamespaces map[string]bool
	HTTP              *http.Client
	// Run allows subprocess behavior to be exercised without cluster writes.
	Run func(context.Context, []byte, ...string) ([]byte, error)
}
type Target struct {
	ConnectionID, OrgID, Namespace, Release, Backend, Desired string
	Revision                                                  int64
}

func (t Target) HubURL() string {
	return fmt.Sprintf("http://%s-tools.%s.svc.cluster.local:8000", t.Release, t.Namespace)
}
func (t Target) Marker() string { return fmt.Sprintf("%s:%d", t.ConnectionID, t.Revision) }

var label = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

func (a *Runtime) target(ctx context.Context, operationID string) (Target, error) {
	var t Target
	err := a.Pool.QueryRow(ctx, `SELECT c.connection_id,c.org_id,t.namespace,t.release_name,c.hub_backend_name,c.desired_state,c.revision
 FROM tenant_connection_operations o JOIN tenant_connections c USING(connection_id) JOIN tenant_registry t USING(org_id)
 WHERE o.operation_id=$1 AND o.revision=c.revision`, operationID).Scan(&t.ConnectionID, &t.OrgID, &t.Namespace, &t.Release, &t.Backend, &t.Desired, &t.Revision)
	return t, err
}

// Apply serializes all upgrades to a tenant release using a session advisory
// lock. Every attempt renders the complete desired connection set, preserving
// unrelated existing release values. No credentials or Helm output are returned.
func (a *Runtime) Apply(ctx context.Context, operationID string) error {
	t, err := a.target(ctx, operationID)
	if err != nil {
		return err
	}
	if !a.AllowedNamespaces[t.Namespace] {
		return fmt.Errorf("tenant namespace is not enabled for connection automation")
	}
	if !label.MatchString(t.Namespace) || !label.MatchString(t.Release) {
		return fmt.Errorf("invalid release target")
	}
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				activity.RecordHeartbeat(ctx, operationID)
			}
		}
	}()
	conn, err := a.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	key := t.Namespace + "/" + t.Release
	// try-lock loop remains cancellable and heartbeats while another upgrade runs.
	for {
		var locked bool
		if err = conn.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended($1,0))", key).Scan(&locked); err != nil {
			return err
		}
		if locked {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, e := conn.Exec(c, "SELECT pg_advisory_unlock(hashtextextended($1,0))", key); e != nil {
			_ = conn.Conn().Close(c)
		}
	}()
	_, err = conn.Exec(ctx, `UPDATE tenant_connection_operations SET status='running',updated_at=now() WHERE operation_id=$1 AND status IN ('pending','running')`, operationID)
	if err != nil {
		return err
	}
	_, err = conn.Exec(ctx, `UPDATE tenant_connections SET state=CASE WHEN desired_state='connected' THEN 'provisioning' ELSE 'disconnecting' END,updated_at=now() WHERE connection_id=$1`, t.ConnectionID)
	if err != nil {
		return err
	}
	rows, err := conn.Query(ctx, `SELECT c.connection_id,c.revision,c.hub_backend_name,c.desired_state,i.auth_kind,i.manifest_template,COALESCE(k.token,'')
 FROM tenant_connections c JOIN integration_catalog i USING(integration_id)
 LEFT JOIN tenant_connection_credentials k USING(connection_id) WHERE c.org_id=$1 ORDER BY c.integration_id`, t.OrgID)
	if err != nil {
		return err
	}
	manifests := map[string]any{}
	for rows.Next() {
		var id, backend, desired, auth, token string
		var rev int64
		var raw []byte
		if err = rows.Scan(&id, &rev, &backend, &desired, &auth, &raw, &token); err != nil {
			rows.Close()
			return err
		}
		callback := strings.TrimRight(a.PublicURL, "/") + "/hub/" + url.PathEscape(t.OrgID) + "/oauth/" + url.PathEscape(backend) + "/callback"
		entry, e := RenderManifest(raw, backend, fmt.Sprintf("%s:%d", id, rev), desired, auth, token, callback)
		if e != nil {
			rows.Close()
			return e
		}
		manifests[backend+".yaml"] = entry
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	// JSON is valid YAML; Helm accepts it on stdin. Secret values never appear
	// in argv, activity arguments/results, or command-error output.
	override := map[string]any{"mcp-hub": map[string]any{"manifests": manifests}}
	data, err := json.Marshal(override)
	if err != nil {
		return err
	}
	run := a.Run
	if run == nil {
		run = RunHelm
	}
	// Enforce the baked chart version to avoid upgrading unrelated components
	// accidentally when the connection worker image changes.
	meta, err := run(ctx, nil, "get", "metadata", t.Release, "-n", t.Namespace, "-o", "json")
	if err != nil {
		return err
	}
	chart, err := run(ctx, nil, "show", "chart", a.ChartDir)
	if err != nil {
		return err
	}
	if err = CheckChart(meta, chart); err != nil {
		return err
	}
	existing, err := run(ctx, nil, "get", "values", t.Release, "-n", t.Namespace, "-o", "json")
	if err != nil {
		return err
	}
	if !ContainsOverride(existing, data) {
		_, err = run(ctx, data, "upgrade", t.Release, a.ChartDir, "-n", t.Namespace, "--reuse-values", "-f", "-", "--wait", "--timeout", "5m")
		if err != nil {
			return err
		}
	}
	return nil
}

// RenderManifest accepts reviewed catalog definitions only, and overwrites
// identity/auth from typed tenant fields. A null entry removes a managed backend
// under Helm's map merge without removing other manually installed backends.
func RenderManifest(raw []byte, backend, marker, desired, auth, token, callback string) (any, error) {
	if !label.MatchString(backend) {
		return nil, fmt.Errorf("invalid backend name")
	}
	if desired == "disconnected" {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || m == nil {
		return nil, fmt.Errorf("invalid integration template")
	}
	transport, ok := m["transport"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing integration transport")
	}
	endpoint, _ := transport["url"].(string)
	u, e := url.Parse(endpoint)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return nil, fmt.Errorf("invalid integration endpoint")
	}
	m["name"] = backend
	m["connection_revision"] = marker
	switch auth {
	case "oauth":
		settings, ok := m["auth"].(map[string]any)
		if !ok {
			settings = map[string]any{}
		}
		settings["type"], settings["redirect_uri"] = "oauth", callback
		m["auth"] = settings
	case "header":
		if token == "" {
			return nil, fmt.Errorf("access token required")
		}
		m["auth"] = map[string]any{"type": "header", "headers": map[string]string{"Authorization": "Bearer " + token}}
	case "none":
		delete(m, "auth")
	default:
		return nil, fmt.Errorf("unsupported catalog authentication kind")
	}
	b, err := yaml.Marshal(m)
	return string(b), err
}

func CheckChart(metadata, chart []byte) error {
	var m struct {
		Chart   string `json:"chart"`
		Version string `json:"version"`
		Status  string `json:"status"`
	}
	var c struct {
		Name    string `yaml:"name"`
		Version string `yaml:"version"`
	}
	if json.Unmarshal(metadata, &m) != nil || yaml.Unmarshal(chart, &c) != nil || m.Chart != c.Name || m.Version != c.Version || c.Name == "" {
		return fmt.Errorf("installed tenant chart differs from the automation image; use a matching chart build")
	}
	if m.Status != "deployed" {
		return fmt.Errorf("tenant release requires operator attention before upgrade")
	}
	return nil
}
func ContainsOverride(existing, desired []byte) bool {
	var a, b map[string]any
	if json.Unmarshal(existing, &a) != nil || json.Unmarshal(desired, &b) != nil {
		return false
	}
	var contains func(any, any) bool
	contains = func(x, y any) bool {
		ym, ok := y.(map[string]any)
		if !ok {
			xb, _ := json.Marshal(x)
			yb, _ := json.Marshal(y)
			return bytes.Equal(xb, yb)
		}
		xm, ok := x.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range ym {
			if !contains(xm[k], v) {
				return false
			}
		}
		return true
	}
	return contains(a, b)
}
func RunHelm(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "helm", args...)
	cmd.Stdin = bytes.NewReader(stdin)
	// Helm failure output can contain rendered secrets. Keep it out of logs and history.
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("helm %s failed (inspect release status with operator credentials)", args[0])
	}
	return out, nil
}

type BackendStatus struct {
	Name      string `json:"name"`
	State     string `json:"state"`
	Revision  string `json:"revision"`
	ToolCount int    `json:"tool_count"`
}

func (a *Runtime) ReadStatus(ctx context.Context, t Target) (string, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.HubURL()+"/api/backends", nil)
	if err != nil {
		return "", 0, err
	}
	client := a.HTTP
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	res, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", 0, fmt.Errorf("hub status unavailable")
	}
	var body struct {
		Items []BackendStatus `json:"items"`
	}
	if err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&body); err != nil {
		return "", 0, err
	}
	for _, b := range body.Items {
		if b.Name == t.Backend {
			if t.Desired == "disconnected" || b.Revision != t.Marker() {
				return "indexing", 0, nil
			}
			switch b.State {
			case "ready", "needs_authorization", "failed", "indexing":
				return b.State, b.ToolCount, nil
			default:
				return "indexing", 0, nil
			}
		}
	}
	if t.Desired == "disconnected" {
		return "disconnected", 0, nil
	}
	return "indexing", 0, nil
}

func (a *Runtime) Observe(ctx context.Context, operationID string) (string, error) {
	t, err := a.target(ctx, operationID)
	if err != nil {
		return "", err
	}
	state, count, err := a.ReadStatus(ctx, t)
	if err != nil {
		return "", err
	}
	visibleState := state
	if state == "failed" {
		visibleState = "indexing"
	} // workflow is still retrying
	if t.Desired == "disconnected" && state != "disconnected" {
		visibleState = "disconnecting"
	}
	_, err = a.Pool.Exec(ctx, `UPDATE tenant_connections SET state=$2,tool_count=$3,observed_at=now(),updated_at=now() WHERE connection_id=$1 AND revision=$4`, t.ConnectionID, visibleState, count, t.Revision)
	return state, err
}
func (a *Runtime) Finish(ctx context.Context, operationID string, failed bool) error {
	tx, err := a.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	status, msg := "completed", ""
	if failed {
		status, msg = "failed", "Connection setup failed. Retry or ask your workspace operator to inspect the release."
	}
	result, err := tx.Exec(ctx, `UPDATE tenant_connection_operations SET status=$2,error=NULLIF($3,''),updated_at=now() WHERE operation_id=$1 AND status IN ('pending','running')`, operationID, status, msg)
	if err != nil {
		return err
	}
	if result.RowsAffected() == 0 {
		return tx.Commit(ctx)
	}
	if failed {
		_, err = tx.Exec(ctx, `UPDATE tenant_connections c SET state='failed',error=$2,updated_at=now() FROM tenant_connection_operations o WHERE o.operation_id=$1 AND o.connection_id=c.connection_id AND o.revision=c.revision`, operationID, msg)
		if err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Refresh observes connected systems after the provisioning workflow ends,
// including OAuth completion and later outages. It never mutates desired state.
func (a *Runtime) Refresh(ctx context.Context) error {
	rows, err := a.Pool.Query(ctx, `SELECT c.connection_id,c.org_id,t.namespace,t.release_name,c.hub_backend_name,c.desired_state,c.revision
 FROM tenant_connections c JOIN tenant_registry t USING(org_id) WHERE c.desired_state='connected'
 AND NOT EXISTS(SELECT 1 FROM tenant_connection_operations o WHERE o.connection_id=c.connection_id AND o.status IN ('pending','running'))
 AND c.state IN ('needs_authorization','ready','indexing','failed')`)
	if err != nil {
		return err
	}
	var targets []Target
	for rows.Next() {
		var t Target
		if err = rows.Scan(&t.ConnectionID, &t.OrgID, &t.Namespace, &t.Release, &t.Backend, &t.Desired, &t.Revision); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	group, ctx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for _, t := range targets {
		if !a.AllowedNamespaces[t.Namespace] {
			continue
		}
		group.Go(func() error {
			state, count, e := a.ReadStatus(ctx, t)
			if e != nil {
				state, count = "failed", 0
			}
			_, err := a.Pool.Exec(ctx, `UPDATE tenant_connections SET state=$2,tool_count=$3,observed_at=now(),updated_at=now(),error=CASE WHEN $2='failed' THEN 'Service is unavailable; reconnect or retry.' ELSE NULL END
  WHERE connection_id=$1 AND revision=$4 AND NOT (state='failed' AND $2='indexing') AND NOT EXISTS(SELECT 1 FROM tenant_connection_operations WHERE connection_id=$1 AND status IN ('pending','running'))`, t.ConnectionID, state, count, t.Revision)
			if err != nil {
				return err
			}
			return nil
		})
	}
	return group.Wait()
}
