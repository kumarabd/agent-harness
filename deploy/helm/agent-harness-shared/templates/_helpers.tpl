{{/*
Base name for resources, honoring nameOverride/fullnameOverride if the user
sets one. Standard Helm chart convention.
*/}}
{{- define "agent-harness-shared.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "agent-harness-shared.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "agent-harness-shared.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Common labels, applied to every resource in this chart. No per-component
label helpers here (unlike agent-harness-tenant) — this chart has exactly
one workload.
*/}}
{{- define "agent-harness-shared.labels" -}}
helm.sh/chart: {{ include "agent-harness-shared.chart" . }}
{{ include "agent-harness-shared.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{/*
Selector labels — kept separate from the full label set because these must
never change across releases (Deployment selectors are immutable).
*/}}
{{- define "agent-harness-shared.selectorLabels" -}}
app.kubernetes.io/name: {{ include "agent-harness-shared.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
