package activities

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"time"

	"go.temporal.io/sdk/activity"
)

// runCommand and runCommandStdin shell out to helm/kubectl — the
// provisioning mechanism docs/components/gateway/web.md's Phase 2 section
// deliberately chose over a k8s.io/client-go or helm.sh/helm/v3 Go SDK
// dependency (neither exists anywhere in this module today): this executes
// literally the same command sequence deploy/helm/tenants/README.md
// already documents as the manual process, so there's no risk of subtly
// reimplementing Helm's own merge/templating semantics. ctx cancellation
// kills the subprocess; activity.RecordHeartbeat fires every 10s so a long
// `helm upgrade --install` (the tenant chart pulls three real dependencies)
// doesn't trip Temporal's own activity heartbeat timeout.

func runCommand(ctx context.Context, name string, args ...string) (string, error) {
	return run(ctx, exec.CommandContext(ctx, name, args...))
}

// runCommandStdin is runCommand plus piping stdin — e.g. `kubectl apply -f
// -`, `helm upgrade --install -f -`.
func runCommandStdin(ctx context.Context, stdin string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = bytes.NewBufferString(stdin)
	return run(ctx, cmd)
}

func run(ctx context.Context, cmd *exec.Cmd) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	heartbeatCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				// Program name only, never cmd.Args — defense in depth
				// alongside secrets.go's own "secrets go via stdin, never a
				// CLI argument" discipline: activity.RecordHeartbeat details
				// are recorded in Temporal's event history exactly like an
				// activity's input/output, so this must never risk echoing
				// a secret value some future caller passed as an arg.
				activity.RecordHeartbeat(ctx, fmt.Sprintf("running: %s", cmd.Path))
			}
		}
	}()

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%v: %w\nstdout: %s\nstderr: %s", cmd.Args, err, stdout.String(), stderr.String())
	}
	return stdout.String(), nil
}
