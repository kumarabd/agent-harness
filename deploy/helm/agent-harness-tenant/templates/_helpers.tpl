{{/*
Base name for resources, honoring nameOverride/fullnameOverride if the user
sets one. Standard Helm chart convention.
*/}}
{{- define "agent-harness.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Only meaningful as a NAME PREFIX when fullnameOverride is explicitly set —
see agent-harness.resourceName below for why a prefix isn't the default.
Empty when unset (deliberately NOT .Release.Name — this used to default to
the release name / tenant slug, which is exactly the prefix
agent-harness.resourceName now omits by default).
*/}}
{{- define "agent-harness.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "agent-harness.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels, applied to every resource in this chart.
*/}}
{{- define "agent-harness.labels" -}}
helm.sh/chart: {{ include "agent-harness.chart" . }}
{{ include "agent-harness.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — kept separate from the full label set because these must
never change across releases (Deployment selectors are immutable).
*/}}
{{- define "agent-harness.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-harness.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/*
Bare resource name (e.g. "worker", "config", "postgres-migrate") by default,
no tenant-slug/release-name prefix. 2026-09-27: this chart is meant to be
installed one release per tenant into that tenant's OWN dedicated Kubernetes
namespace (deploy/helm/tenants/README.md), so the namespace itself already
makes "worker" unambiguous — a "<tenant>-worker" prefix was pure redundancy,
not a real collision guard, for every tenant provisioned that way (which is
every tenant the self-serve automation creates).
fullnameOverride is the escape hatch for the one kind of tenant this doesn't
hold for: one installed into a Kubernetes namespace it SHARES with other
releases (deploy/helm/tenants/abishekk.yaml, a pre-this-convention tenant
that still lives in the "agents" namespace alongside the shared router/web —
see that file's own comment). Set fullnameOverride there and every name this
helper produces goes back to being prefixed with it, exactly as it always
was for that tenant — this is what makes it safe to upgrade that tenant with
this same chart without an unwanted Postgres StatefulSet/PVC rename (a new
StatefulSet name gets a new, empty PVC — the old one holding real data would
be orphaned and, this cluster's reclaim policy being Delete, garbage
collected).
agent-harness.fullname (the tenant slug) is still used directly, unprefixed,
where the value itself needs to BE the tenant slug regardless of any of
this (TENANT_SLUG, the Temporal task queue name) — those were never part of
this redundant-prefix problem.
*/}}
{{- define "agent-harness.componentFullname" -}}
{{- $prefix := include "agent-harness.fullname" .context -}}
{{- if $prefix -}}
{{- printf "%s-%s" $prefix .component -}}
{{- else -}}
{{- .component -}}
{{- end -}}
{{- end -}}

{{/*
Same escape-hatch logic as agent-harness.componentFullname above, for a
literal resource-name suffix instead of a "component" (Secrets, ConfigMaps,
Jobs, the PVC, the ServiceAccount — anything not a Deployment/Service).
*/}}
{{- define "agent-harness.resourceName" -}}
{{- $prefix := include "agent-harness.fullname" .context -}}
{{- if $prefix -}}
{{- printf "%s-%s" $prefix .name -}}
{{- else -}}
{{- .name -}}
{{- end -}}
{{- end -}}

{{/*
Cross-subchart references: this tenant's Postgres/mcp-hub/agent-brain
Service and Secret names, tracking whatever that subchart's OWN
(fullname)Override actually resolves to (values.yaml) rather than a second,
hardcoded copy of the same decision. Every template that needs to reach one
of these subcharts' resources directly (env vars, the migrate/init-roles
hook Jobs) uses these, so abishekk.yaml's escape-hatch overrides (see
componentFullname's own comment) only ever need to be set in ONE place.
*/}}
{{- define "agent-harness.postgresqlName" -}}
{{- .Values.postgresql.fullnameOverride | default "postgresql" -}}
{{- end -}}

{{- define "agent-harness.mcpHubName" -}}
{{- (index .Values "mcp-hub" "fullnameOverride") | default "tools" -}}
{{- end -}}

{{- define "agent-harness.agentBrainName" -}}
{{- (index .Values "agent-brain" "fullnameOverride") | default "memory" -}}
{{- end -}}

{{- define "agent-harness.componentSelectorLabels" -}}
{{ include "agent-harness.selectorLabels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "agent-harness.componentLabels" -}}
{{ include "agent-harness.labels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/*
maps-engine's database password. Explicit mapsEngine.postgres.password wins; otherwise it is derived from the tenant's
Postgres password, so it is stable across upgrades (the init-roles hook creates the role once and never changes its
password) and needs no per-tenant configuration. Used by both the Secret and the hook, so they cannot disagree.
*/}}
{{- define "agent-harness.mapsEnginePassword" -}}
{{- if .Values.mapsEngine.postgres.password -}}
{{- .Values.mapsEngine.postgres.password -}}
{{- else if .Values.postgresql.auth.postgresPassword -}}
{{- printf "%s:maps-engine-db" .Values.postgresql.auth.postgresPassword | sha256sum | trunc 40 -}}
{{- else -}}
{{- fail "maps-engine needs a database password: set postgresql.auth.postgresPassword (the maps database password is derived from it) or mapsEngine.postgres.password, or set mapsEngine.enabled=false for this tenant." -}}
{{- end -}}
{{- end -}}

{{/* The one user maps-engine serves: mapsEngine.ownerSub, else the tenant's own agentBrain.ownerUserID. */}}
{{- define "agent-harness.mapsEngineOwner" -}}
{{- .Values.mapsEngine.ownerSub | default .Values.agentBrain.ownerUserID -}}
{{- end -}}
