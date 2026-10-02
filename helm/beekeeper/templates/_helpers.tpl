{{/*
The name of every object: the release's, as the central instance has one
installation per cluster.
*/}}
{{- define "beekeeper.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
The chart label. A label value is at most 63 characters and begins and ends
alphanumeric: the cut of a long version (a branch build's
<version>-dev.<branch>.<date>.<time>.<sha>, or the <version>+<digest>
helm-controller installs) can land on any run of ".", "_" and "-".
*/}}
{{- define "beekeeper.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimAll "-._" }}
{{- end }}

{{- define "beekeeper.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "beekeeper.labels" -}}
helm.sh/chart: {{ include "beekeeper.chart" . }}
{{ include "beekeeper.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
application.giantswarm.io/team: {{ index .Chart.Annotations "io.giantswarm.application.team" | quote }}
{{- end }}

{{/*
The team namespaces, beekeeper-<team> with the team made a DNS label as the
store makes it (lower case, every other run "-", at most 40 characters): one
per distinct team of serve.teams.
*/}}
{{- define "beekeeper.teamNamespaces" -}}
{{- $out := list }}
{{- range $group, $team := .Values.serve.teams }}
{{- $label := regexReplaceAll "[^a-z0-9-]+" ($team | lower) "-" | trimAll "-" | trunc 40 | trimAll "-" | default "x" }}
{{- $out = append $out (printf "beekeeper-%s" $label) }}
{{- end }}
{{- $out | uniq | sortAlpha | toJson }}
{{- end }}
