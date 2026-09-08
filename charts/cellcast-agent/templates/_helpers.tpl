{{- define "cellcast-agent.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cellcast-agent.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{- define "cellcast-agent.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cellcast-agent.labels" -}}
helm.sh/chart: {{ include "cellcast-agent.chart" . }}
{{ include "cellcast-agent.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cellcast
{{- end }}

{{- define "cellcast-agent.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cellcast-agent.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: agent
{{- end }}

{{- define "cellcast-agent.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cellcast-agent.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "cellcast-agent.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s/%s@%s" .Values.image.registry .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end }}

{{/*
The two values with no defensible default.

An agent with no cell name reports for nothing, and one with no hub endpoint
reports to nothing. Both fail at startup anyway; failing at install time says so
before the pod exists, and says which value is missing.
*/}}
{{- define "cellcast-agent.validateRequired" -}}
{{- if not .Values.cellName -}}
{{- fail "cellName must be set to the name of this cell's Cluster resource in the hub's registry" -}}
{{- end -}}
{{- if not .Values.hub.endpoint -}}
{{- fail "hub.endpoint must be set to the URL of the cellcast hub" -}}
{{- end -}}
{{- end }}
