{{- define "dup-helpers.name" -}}
{{- .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "dup-helpers.labels" -}}
app.kubernetes.io/name: {{ include "dup-helpers.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}
