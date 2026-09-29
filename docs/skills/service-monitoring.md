# Service-monitoring skill

Service monitoring uses the [generic conversational skill contract](../05-architecture-domain-control-loops.md). The user selects `/skill service_monitoring` (tenant opt-in remains required). Ordinary chat gathers the target and notification condition, submits the proposal, and obtains exact-proposal approval before a bounded skill step starts.

## Domain work

The skill discovers allowlisted Grafana read tools and inspects real telemetry, dashboards or existing alerts. It cannot substitute another backend, change Grafana configuration, or bypass a tenant permission rule.

After successful Grafana evidence, it can call its native `create_intention` handler. Validation requires a one-shot `condition` intention whose probe uses the concrete Grafana server/tool actually inspected. The domain prompt requires the target and predicate to match the approved request; interpreting telemetry and constructing that predicate remain model-directed.

Completion requires `armed=true` and an intention ID. An already-existing matching objective is not proof that this proposal was newly armed. Ordinary chat reports the verified result and retains the user's selected mode.

## Intention versus setup step

The skill child performs setup. The resulting `IntentionWorkflow` owns the durable wait/probe/fire lifecycle separately. Stopping the setup step or leaving the selected mode does not cancel an already-armed intention; ordinary intention-management tools own that operation.

This implementation supports a one-shot condition notification. Recurring reviews, automatic Grafana alert provisioning and push-webhook integration are not implemented by this skill; it must explain the limitation instead of promising them. Read-only Grafana discovery and a one-shot intention are deliberately the current bounded tool policy.

Domain logic and native-handler binding: `activities/activities/skills/service_monitoring.py`. Shared execution: `skill_runtime.py` and `workflows/internal/workflow/skill.go`. Intention execution remains `tools_intention.py` and `workflows/internal/workflow/intention.go`.
