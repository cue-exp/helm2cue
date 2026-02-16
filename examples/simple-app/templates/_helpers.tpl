{{- define "simple-app.fullname" -}}
{{- .Release.Name -}}
{{- end -}}

{{- define "simple-app.labels" -}}
app: {{ .Values.name }}
chart: {{ .Chart.Name }}
{{- end -}}
