{{/*
Expand the name of the chart.
*/}}
{{- define "conduit-kubelet.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name (max 63 chars).
*/}}
{{- define "conduit-kubelet.fullname" -}}
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

{{/*
Chart label value.
*/}}
{{- define "conduit-kubelet.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "conduit-kubelet.labels" -}}
helm.sh/chart: {{ include "conduit-kubelet.chart" . }}
{{ include "conduit-kubelet.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "conduit-kubelet.selectorLabels" -}}
app.kubernetes.io/name: {{ include "conduit-kubelet.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Namespace the kubelet runs in.
*/}}
{{- define "conduit-kubelet.namespace" -}}
{{- default .Release.Namespace .Values.kubelet.namespace }}
{{- end }}

{{/*
Service account name.
*/}}
{{- define "conduit-kubelet.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "conduit-kubelet.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Name of the Secret holding CONDUIT_API_TOKEN (and optionally RUNPOD_API_KEY).
*/}}
{{- define "conduit-kubelet.secretName" -}}
{{- default (include "conduit-kubelet.fullname" .) .Values.conduit.existingSecret }}
{{- end }}

{{/*
Port of the health server, derived from kubelet.healthServerAddress (":8080" -> 8080).
*/}}
{{- define "conduit-kubelet.healthPort" -}}
{{- .Values.kubelet.healthServerAddress | toString | splitList ":" | last }}
{{- end }}
