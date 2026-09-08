{{/*
Expand the name of the chart.
*/}}
{{- define "cellcast.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified app name.
*/}}
{{- define "cellcast.fullname" -}}
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

{{- define "cellcast.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "cellcast.labels" -}}
helm.sh/chart: {{ include "cellcast.chart" . }}
{{ include "cellcast.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: cellcast
{{- end }}

{{- define "cellcast.selectorLabels" -}}
app.kubernetes.io/name: {{ include "cellcast.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/component: hub
{{- end }}

{{- define "cellcast.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "cellcast.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
The namespace holding the registry. Empty means the release namespace, which is
what a single-tenant install wants.
*/}}
{{- define "cellcast.registryNamespace" -}}
{{- default .Release.Namespace .Values.hub.registryNamespace }}
{{- end }}

{{- define "cellcast.leaderElectionNamespace" -}}
{{- default .Release.Namespace .Values.hub.leaderElection.namespace }}
{{- end }}

{{/*
The image reference. A digest wins over a tag, because a digest is the only one
of the two that names the same bytes tomorrow.
*/}}
{{- define "cellcast.image" -}}
{{- $registry := .Values.image.registry -}}
{{- $repository := .Values.image.repository -}}
{{- if .Values.image.digest -}}
{{- printf "%s/%s@%s" $registry $repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s/%s:%s" $registry $repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end }}

{{/*
Seconds in a Go duration string, for the arithmetic the grace period depends on.
Accepts the subset the hub's own flags accept: an integer with an h, m, s or ms
suffix. Anything else is a value the chart cannot check, so it says so rather
than guessing.
*/}}
{{- define "cellcast.durationSeconds" -}}
{{- $d := . | toString -}}
{{- if hasSuffix "ms" $d -}}
{{- div (int64 (trimSuffix "ms" $d)) 1000 -}}
{{- else if hasSuffix "s" $d -}}
{{- int64 (trimSuffix "s" $d) -}}
{{- else if hasSuffix "m" $d -}}
{{- mul (int64 (trimSuffix "m" $d)) 60 -}}
{{- else if hasSuffix "h" $d -}}
{{- mul (int64 (trimSuffix "h" $d)) 3600 -}}
{{- else -}}
{{- fail (printf "cannot read %q as a duration; use an integer with an h, m, s or ms suffix" $d) -}}
{{- end -}}
{{- end }}

{{/*
Refuse to render a hub whose shutdown cannot finish.

drainDelay and shutdownTimeout run one after the other, so what the kubelet has
to accommodate is their sum. A grace period shorter than that means SIGKILL
lands mid-drain, which turns the graceful shutdown into the ungraceful one it
exists to replace, and nothing about the running pod would say so.
*/}}
{{- define "cellcast.validateShutdown" -}}
{{- $drain := int (include "cellcast.durationSeconds" .Values.hub.drainDelay) -}}
{{- $shutdown := int (include "cellcast.durationSeconds" .Values.hub.shutdownTimeout) -}}
{{- $grace := int .Values.terminationGracePeriodSeconds -}}
{{- if le $grace (add $drain $shutdown) -}}
{{- fail (printf "terminationGracePeriodSeconds (%d) must exceed hub.drainDelay (%ds) plus hub.shutdownTimeout (%ds); the kubelet would kill the pod mid-drain" $grace $drain $shutdown) -}}
{{- end -}}
{{- end }}
