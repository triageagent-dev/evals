{{- define "agentevals-go.labels" -}}
app: agentevals-go
app.kubernetes.io/name: agentevals-go
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ (.Values.image.tag | default .Chart.AppVersion) | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}
