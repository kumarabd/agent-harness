// connections dispatches durable connection requests and runs their workflows
// independently of tenant onboarding and agent-loop workers.
package main

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"agent-harness/workflows/internal/automation"
	"agent-harness/workflows/internal/automation/connections"
	"github.com/jackc/pgx/v5/pgxpool"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	db := url.URL{Scheme: "postgres", Host: env("POSTGRES_HOST", "localhost") + ":" + env("POSTGRES_PORT", "5432"), Path: "/" + env("POSTGRES_DB", "router"), User: url.UserPassword(env("POSTGRES_USER", "router"), os.Getenv("POSTGRES_PASSWORD"))}
	pool, err := pgxpool.New(ctx, db.String())
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	public := strings.TrimRight(os.Getenv("ROUTER_PUBLIC_URL"), "/")
	u, e := url.Parse(public)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		log.Fatal("ROUTER_PUBLIC_URL must be an absolute HTTP(S) URL")
	}
	allowed := map[string]bool{}
	for _, n := range strings.Split(os.Getenv("AUTOMATION_TENANT_NAMESPACES"), ",") {
		if n = strings.TrimSpace(n); n != "" {
			allowed[n] = true
		}
	}
	if len(allowed) == 0 {
		log.Fatal("AUTOMATION_TENANT_NAMESPACES is required")
	}
	tc, err := client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "localhost:7233"), Namespace: env("TEMPORAL_NAMESPACE", "system")})
	if err != nil {
		log.Fatal(err)
	}
	defer tc.Close()
	queue := env("TEMPORAL_TASK_QUEUE", "connection-automation")
	runtime := &connections.Runtime{Pool: pool, ChartDir: env("TENANT_CHART_DIR", "/charts/tenant"), PublicURL: public, AllowedNamespaces: allowed}
	w := worker.New(tc, queue, worker.Options{MaxConcurrentActivityExecutionSize: 4})
	w.RegisterWorkflow(automation.ConnectionProvisionWorkflow)
	w.RegisterActivityWithOptions(runtime.Apply, activity.RegisterOptions{Name: "ApplyConnection"})
	w.RegisterActivityWithOptions(runtime.Observe, activity.RegisterOptions{Name: "ObserveConnection"})
	w.RegisterActivityWithOptions(runtime.Finish, activity.RegisterOptions{Name: "FinishConnection"})
	if err = w.Start(); err != nil {
		log.Fatal(err)
	}
	defer w.Stop()
	go func() {
		for ctx.Err() == nil {
			if e := runtime.Refresh(ctx); e != nil && ctx.Err() == nil {
				log.Printf("connection observation failed")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		round, cancel := context.WithTimeout(ctx, time.Minute)
		err = dispatch(round, pool, tc, queue)
		cancel()
		if err != nil && ctx.Err() == nil {
			log.Printf("connection dispatch: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Pending operations are the outbox. A lost StartWorkflow response is harmless:
// every operation has a fixed, non-reusable workflow ID.
func dispatch(ctx context.Context, pool *pgxpool.Pool, tc client.Client, queue string) error {
	rows, err := pool.Query(ctx, "SELECT operation_id::text,connection_id::text,status FROM tenant_connection_operations WHERE status IN ('pending','running') ORDER BY updated_at LIMIT 100")
	if err != nil {
		return err
	}
	var pending []automation.ConnectionProvisionInput
	states := map[string]string{}
	for rows.Next() {
		var in automation.ConnectionProvisionInput
		var state string
		if err = rows.Scan(&in.OperationID, &in.ConnectionID, &state); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, in)
		states[in.OperationID] = state
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, in := range pending {
		if states[in.OperationID] == "pending" {
			_, err = tc.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: "connection-" + in.OperationID, TaskQueue: queue, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, automation.ConnectionProvisionWorkflow, in)
			var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
			if err != nil && !errors.As(err, &duplicate) {
				return err
			}
		}
		description, e := tc.DescribeWorkflowExecution(ctx, "connection-"+in.OperationID, "")
		if e != nil {
			return e
		}
		if description.WorkflowExecutionInfo.Status != enumspb.WORKFLOW_EXECUTION_STATUS_RUNNING {
			if e = (&connections.Runtime{Pool: pool}).Finish(ctx, in.OperationID, true); e != nil {
				return e
			}
		} else {
			if _, e = pool.Exec(ctx, "UPDATE tenant_connection_operations SET updated_at=now() WHERE operation_id=$1 AND status IN ('pending','running')", in.OperationID); e != nil {
				return e
			}
		}
	}
	return nil
}
