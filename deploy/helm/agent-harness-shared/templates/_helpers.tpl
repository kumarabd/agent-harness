{{/*
Base name for resources, honoring nameOverride/fullnameOverride if the user
sets one. Standard Helm chart convention.
*/}}
{{- define "agent-harness.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
This chart is installed ONCE for the whole cluster, with the release name
conventionally set to "harness" (docs/components/multi-tenancy.md) — so the
base name for every resource here is just the release name itself, no
chart-name suffix appended. fullnameOverride is still honored as the
standard escape hatch.
*/}}
{{- define "agent-harness.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{- define "agent-harness.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels, applied to every resource in this chart. No per-component
label helpers here (unlike agent-harness-tenant) — this chart has exactly
one workload.
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
