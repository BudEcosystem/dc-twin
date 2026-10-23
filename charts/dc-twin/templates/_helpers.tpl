{{- define "dc-twin.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "dc-twin.fullname" -}}
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

{{- define "dc-twin.labels" -}}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{ include "dc-twin.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "dc-twin.selectorLabels" -}}
app.kubernetes.io/name: {{ include "dc-twin.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "dc-twin.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "dc-twin.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "dc-twin.image" -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) }}
{{- end }}

{{/* The ConfigMap the estate file is mounted from, if any: the user's, or the chart's for estate.file */}}
{{- define "dc-twin.estateConfigMap" -}}
{{- if .Values.estate.existingConfigMap }}
{{- .Values.estate.existingConfigMap }}
{{- else if .Values.estate.file }}
{{- printf "%s-estate" (include "dc-twin.fullname" .) }}
{{- end }}
{{- end }}

{{/* The estate file's path in the container */}}
{{- define "dc-twin.estatePath" -}}
{{- if .Values.estate.existingConfigMap }}
{{- printf "/etc/dctwin/estate/%s" .Values.estate.key }}
{{- else if .Values.estate.file }}
{{- print "/etc/dctwin/estate/estate.yaml" }}
{{- else }}
{{- printf "/opt/dctwin/examples/estates/%s.yaml" (required "set estate.file, estate.existingConfigMap or estate.example" .Values.estate.example) }}
{{- end }}
{{- end }}

{{/* Whether the control API listens beyond the pod's loopback (D54) */}}
{{- define "dc-twin.controlExposed" -}}
{{- if or .Values.control.service.enabled .Values.ingress.enabled }}true{{ end }}
{{- end }}

{{/* The Secret that holds the estate token, and its key */}}
{{- define "dc-twin.tokenSecret" -}}
{{- default (printf "%s-token" (include "dc-twin.fullname" .)) .Values.control.token.existingSecret }}
{{- end }}
{{- define "dc-twin.tokenKey" -}}
{{- if .Values.control.token.existingSecret }}{{ .Values.control.token.key }}{{ else }}token{{ end }}
{{- end }}
