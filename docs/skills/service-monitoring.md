# Service monitoring skill loop

`service_monitoring` is the control loop for configuring a monitoring
commitment for one Kubernetes service. The user can explicitly request it, or
the agent can propose it implicitly; either route requires confirmation before
the skill begins setup. Grafana is mandatory as the evidence source: a
Kubernetes, Prometheus, or other tool cannot substitute for a Grafana-derived
signal.

## Logical loop

```mermaid
flowchart TD
    Start([M or user: monitoring request]) --> Scope[M: Identify service, namespace, cluster, and notification intent]
    Scope --> SetupApproval{D: Setup confirmation recorded?}
    SetupApproval -- no --> Stop([D: Close without monitoring])
    SetupApproval -- yes --> Grafana[M: Discover and inspect Grafana]
    Grafana --> Evidence{Trustworthy Grafana signal available?}
    Evidence -- no --> Blocked([Explain the blocker; do not arm monitoring])
    Evidence -- yes --> Decide[Choose the simplest suitable mechanism]

    Decide --> Existing{Existing Grafana alert fits?}
    Existing -- yes --> Connect[Connect the commitment to that signal]
    Existing -- no --> Query{A Grafana query can be monitored?}
    Query -- yes --> Connect
    Query -- no --> Proposal[Propose a Grafana alert rule and threshold]

    Proposal --> Approval{D: User approves the external change?}
    Approval -- no / expired --> Blocked
    Approval -- yes --> Provision[Create or update the approved Grafana alert]
    Provision --> Verify{Alert works and is scoped correctly?}
    Verify -- no / unclear --> Grafana
    Verify -- yes --> Connect

    Connect --> Arm[Create the durable intention or recurring review]
    Arm --> Armed{Commitment armed?}
    Armed -- no / unclear --> Diagnose[Inspect the failure or explain the blocker]
    Diagnose --> Grafana
    Armed -- yes --> Report([Report evidence, mechanism, and commitment])
```

`D` = deterministic workflow behavior. `M` = a model-directed step inside the
scoped reasoning bridge. The initial setup confirmation and any later alert
configuration approval are durable waits; the human supplies the choice.

The main cycle is **inspect Grafana → make a decision from real evidence →
verify → inspect again when necessary**. It is intentionally not a generic
“look at every available observability system” loop: Grafana is required. The
flow stops rather than substituting another source when Grafana cannot
establish a trustworthy signal.

## Logical stages

| Stage | Intent |
|---|---|
| Scope the target | Establish the service, namespace, cluster, and the condition that should notify the user. |
| Gather evidence | Use Grafana to find the service’s actual telemetry, dashboards, queries, and existing alerts. |
| Select a mechanism | Reuse a fitting alert when possible; otherwise monitor a concrete query; create an alert rule only when neither is sufficient. |
| Obtain approval | Before a change to Grafana alerting configuration, show the proposed signal and threshold and wait for explicit approval. |
| Provision and verify | Apply only the approved change, then confirm that its target and behavior match the request. |
| Arm the commitment | Turn the selected Grafana signal into the generic durable intention or an explicit recurring review. |
| Recover or stop | Return to Grafana on ambiguous evidence or a failed verification; stop with a clear blocker when no safe path exists. |

## Relationship to an intention

The service-monitoring skill is the **setup/control loop**. It investigates
Grafana and, only after the chosen mechanism is known, creates the durable
monitoring commitment. The intention is not this workflow: it is a separate
`IntentionWorkflow` execution, or a Temporal Schedule that launches time
intentions for recurring schedules.

This is the intended work loop, not a literal Temporal execution trace. The
first native states are implemented: `ServiceMonitoringSkill` validates the
target and obtains durable setup confirmation itself. After approval, the
current implementation still uses a scoped reasoning bridge for dynamic
Grafana discovery, evidence interpretation, and provisioning. It can author a
Grafana-backed polling intention or explicit recurring review, but it does not
yet turn an arbitrary Grafana alert webhook into a push-triggered intention.
That would be an extension to the generic intention trigger path.

Relevant implementation: `workflows/internal/workflow/skills/service_monitoring.go`,
`workflows/internal/workflow/skills/support.go`,
`workflows/internal/workflow/turn.go`,
`activities/activities/tools_intention.py`, and
`workflows/internal/workflow/intention.go`.
