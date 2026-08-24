{{/*
Base name for resources, honoring nameOverride/fullnameOverride if the user
sets one. Standard Helm chart convention.
*/}}
{{- define "agent-harness.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
This chart is always installed one release per tenant, with the release name
SET TO the tenant name (deploy/helm/tenants/README.md) — so the base name for
every resource here is just the release name itself, no chart-name suffix
appended. fullnameOverride is still honored as the standard escape hatch.
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
Per-component name, e.g. "<tenant>-worker".
*/}}
{{- define "agent-harness.componentFullname" -}}
{{- printf "%s-%s" (include "agent-harness.fullname" .context) .component -}}
{{- end -}}

{{- define "agent-harness.componentSelectorLabels" -}}
{{ include "agent-harness.selectorLabels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{- define "agent-harness.componentLabels" -}}
{{ include "agent-harness.labels" .context }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}
