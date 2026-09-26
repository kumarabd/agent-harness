package registry

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrConnectionBusy = errors.New("connection operation already in progress")
var ErrIntegration = errors.New("integration unavailable")
var ErrCredential = errors.New("access token required")

type Integration struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	Icon        string `json:"icon,omitempty"`
	AuthKind    string `json:"auth_kind"`
}
type Connection struct {
	ID            string    `json:"id"`
	IntegrationID string    `json:"integration_id"`
	DesiredState  string    `json:"desired_state"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	ToolCount     int       `json:"tool_count"`
}
type ConnectionView struct {
	Integration
	Connection *Connection `json:"connection,omitempty"`
}

func (r *Registry) ListConnections(ctx context.Context, orgID string) ([]ConnectionView, error) {
	rows, err := r.pool.Query(ctx, `
 SELECT i.integration_id,i.display_name,i.description,COALESCE(i.icon,''),i.auth_kind,
 c.connection_id,c.desired_state,c.state,COALESCE(c.error,''),c.updated_at,COALESCE(c.tool_count,0)
 FROM integration_catalog i LEFT JOIN tenant_connections c
 ON c.integration_id=i.integration_id AND c.org_id=$1
 WHERE i.enabled OR c.connection_id IS NOT NULL ORDER BY i.display_name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ConnectionView{}
	for rows.Next() {
		var v ConnectionView
		var id *uuid.UUID
		var desired, state, msg *string
		var updated *time.Time
		var count int
		if err := rows.Scan(&v.ID, &v.DisplayName, &v.Description, &v.Icon, &v.AuthKind, &id, &desired, &state, &msg, &updated, &count); err != nil {
			return nil, err
		}
		if id != nil {
			v.Connection = &Connection{ID: id.String(), IntegrationID: v.ID, DesiredState: *desired, State: *state, Error: *msg, UpdatedAt: *updated, ToolCount: count}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// RequestConnection commits desired state, credentials and the dispatch outbox
// together. Locking the tenant row serializes requests even before a connection
// exists; the partial unique index is a second concurrency guard.
func (r *Registry) RequestConnection(ctx context.Context, orgID, integrationID, desired, token string) (Connection, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Connection{}, err
	}
	defer tx.Rollback(ctx)
	var tenant string
	if err = tx.QueryRow(ctx, "SELECT org_id FROM tenant_registry WHERE org_id=$1 FOR UPDATE", orgID).Scan(&tenant); err != nil {
		return Connection{}, err
	}
	var auth string
	err = tx.QueryRow(ctx, "SELECT auth_kind FROM integration_catalog WHERE integration_id=$1 AND (enabled OR $2='disconnected')", integrationID, desired).Scan(&auth)
	if err != nil {
		return Connection{}, ErrIntegration
	}
	var c Connection
	var revision int64
	err = tx.QueryRow(ctx, `SELECT connection_id,integration_id,desired_state,state,COALESCE(error,''),updated_at,tool_count,revision
 FROM tenant_connections WHERE org_id=$1 AND integration_id=$2 FOR UPDATE`, orgID, integrationID).Scan(&c.ID, &c.IntegrationID, &c.DesiredState, &c.State, &c.Error, &c.UpdatedAt, &c.ToolCount, &revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, err
	}
	if err == nil {
		var active bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tenant_connection_operations WHERE connection_id=$1 AND status IN ('pending','running'))", c.ID).Scan(&active); err != nil {
			return Connection{}, err
		}
		if active {
			if c.DesiredState != desired || token != "" {
				return Connection{}, ErrConnectionBusy
			}
			return c, tx.Commit(ctx)
		}
		if c.DesiredState == desired && c.State != "failed" && token == "" {
			return c, tx.Commit(ctx)
		}
	} else {
		if desired == "disconnected" {
			return Connection{}, ErrIntegration
		}
		c.ID = uuid.NewString()
		_, err = tx.Exec(ctx, `INSERT INTO tenant_connections(connection_id,org_id,integration_id,hub_backend_name) VALUES($1,$2,$3,$3)`, c.ID, orgID, integrationID)
		if err != nil {
			return Connection{}, err
		}
	}
	if desired == "connected" && auth == "header" {
		if token != "" {
			_, err = tx.Exec(ctx, `INSERT INTO tenant_connection_credentials(connection_id,token) VALUES($1,$2)
   ON CONFLICT(connection_id) DO UPDATE SET token=EXCLUDED.token,updated_at=now()`, c.ID, token)
			if err != nil {
				return Connection{}, err
			}
		}
		var exists bool
		if err = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM tenant_connection_credentials WHERE connection_id=$1)", c.ID).Scan(&exists); err != nil {
			return Connection{}, err
		}
		if !exists {
			return Connection{}, ErrCredential
		}
	}
	state, kind := "pending", "connect"
	if desired == "disconnected" {
		state, kind = "disconnecting", "disconnect"
	}
	revision++
	err = tx.QueryRow(ctx, `UPDATE tenant_connections SET desired_state=$2,state=$3,error=NULL,revision=$4,updated_at=now()
 WHERE connection_id=$1 RETURNING connection_id,integration_id,desired_state,state,COALESCE(error,''),updated_at,tool_count`,
		c.ID, desired, state, revision).Scan(&c.ID, &c.IntegrationID, &c.DesiredState, &c.State, &c.Error, &c.UpdatedAt, &c.ToolCount)
	if err != nil {
		return Connection{}, err
	}
	opID := uuid.NewString()
	_, err = tx.Exec(ctx, `INSERT INTO tenant_connection_operations(operation_id,connection_id,kind,revision,workflow_id)
 VALUES($1,$2,$3,$4,$5)`, opID, c.ID, kind, revision, "connection-"+opID)
	if err != nil {
		return Connection{}, err
	}
	return c, tx.Commit(ctx)
}

func (r *Registry) OAuthBackend(ctx context.Context, orgID, integrationID string) (string, error) {
	var backend string
	err := r.pool.QueryRow(ctx, `SELECT c.hub_backend_name FROM tenant_connections c JOIN integration_catalog i USING(integration_id)
 WHERE c.org_id=$1 AND c.integration_id=$2 AND c.desired_state='connected' AND i.auth_kind='oauth'`, orgID, integrationID).Scan(&backend)
	return backend, err
}
func (r *Registry) MarshalConnections(ctx context.Context, orgID string) ([]byte, error) {
	items, err := r.ListConnections(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"items": items})
}
