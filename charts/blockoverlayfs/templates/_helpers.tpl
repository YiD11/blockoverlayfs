{{- define "blockoverlayfs.selectorLabels" -}}
app.kubernetes.io/name: blockoverlayfs
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "blockoverlayfs.labels" -}}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{ include "blockoverlayfs.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}
