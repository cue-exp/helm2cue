{{- define "subdir-helpers.operator.labels" -}}
app.kubernetes.io/component: operator
app.kubernetes.io/name: {{ include "subdir-helpers.name" . }}
{{- end -}}
