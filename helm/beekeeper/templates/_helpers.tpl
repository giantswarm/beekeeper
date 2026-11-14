{{/*
The prefix of every object's name: fullnameOverride, else the release's name.
*/}}
{{- define "beekeeper.fullname" -}}
{{- .Values.fullnameOverride | default .Release.Name | trunc 50 | trimSuffix "-" }}
{{- end }}

{{- define "beekeeper.engine.name" -}}
{{- printf "%s-engine" (include "beekeeper.fullname" .) }}
{{- end }}

{{- define "beekeeper.receiver.name" -}}
{{- printf "%s-receiver" (include "beekeeper.fullname" .) }}
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

{{- define "beekeeper.engine.selectorLabels" -}}
{{ include "beekeeper.selectorLabels" . }}
app.kubernetes.io/component: engine
{{- end }}

{{- define "beekeeper.receiver.selectorLabels" -}}
{{ include "beekeeper.selectorLabels" . }}
app.kubernetes.io/component: receiver
{{- end }}

{{- define "beekeeper.engine.labels" -}}
{{ include "beekeeper.labels" . }}
app.kubernetes.io/component: engine
{{- end }}

{{- define "beekeeper.receiver.labels" -}}
{{ include "beekeeper.labels" . }}
app.kubernetes.io/component: receiver
{{- end }}

{{- define "beekeeper.image" -}}
{{- printf "%s/%s:%s" .Values.image.registry .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{/*
The Lease the engine replicas elect their leader with.
*/}}
{{- define "beekeeper.leaderElectionID" -}}
{{- .Values.engine.leaderElectionID | default (include "beekeeper.engine.name" .) }}
{{- end }}

{{/*
Secret names: a value, else the name the chart's convention derives.
*/}}
{{- define "beekeeper.githubApp.secretName" -}}
{{- .Values.github.app.secretName | default (printf "%s-github-app" (include "beekeeper.fullname" .)) }}
{{- end }}

{{- define "beekeeper.webhook.secretName" -}}
{{- .Values.webhook.secretName | default (printf "%s-webhook" (include "beekeeper.fullname" .)) }}
{{- end }}

{{- define "beekeeper.jev.secretName" -}}
{{- .Values.jev.secretName | default (printf "%s-jev" (include "beekeeper.fullname" .)) }}
{{- end }}

{{- define "beekeeper.postgres.cluster" -}}
{{- .Values.postgres.cluster | default (printf "%s-db" (include "beekeeper.fullname" .)) }}
{{- end }}

{{- define "beekeeper.postgres.host" -}}
{{- .Values.postgres.host | default (printf "%s-rw" (include "beekeeper.postgres.cluster" .)) }}
{{- end }}

{{- define "beekeeper.postgres.engine.secretName" -}}
{{- .Values.postgres.engine.secretName | default (printf "%s-app" (include "beekeeper.postgres.cluster" .)) }}
{{- end }}

{{- define "beekeeper.postgres.receiver.secretName" -}}
{{- .Values.postgres.receiver.secretName | default (printf "%s-receiver" (include "beekeeper.postgres.cluster" .)) }}
{{- end }}

{{/*
libpq's environment for one role (pgx reads it): the host, port, database and
sslmode from values, the user and password from the role's Secret.
Call with (dict "root" $ "role" "engine"|"receiver").
*/}}
{{- define "beekeeper.postgres.env" -}}
{{- $role := index .root.Values.postgres .role }}
{{- $secret := include (printf "beekeeper.postgres.%s.secretName" .role) .root -}}
- name: PGHOST
  value: {{ include "beekeeper.postgres.host" .root | quote }}
- name: PGPORT
  value: {{ .root.Values.postgres.port | quote }}
- name: PGDATABASE
  value: {{ .root.Values.postgres.database | quote }}
- name: PGSSLMODE
  value: {{ .root.Values.postgres.sslMode | quote }}
- name: PGUSER
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: {{ $role.usernameKey }}
- name: PGPASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ $secret }}
      key: {{ $role.passwordKey }}
{{- end }}

{{- define "beekeeper.podEnv" -}}
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
{{- end }}

{{/*
A soft anti-affinity that spreads a component's replicas over nodes, unless
the component's affinity is set. Call with (dict "root" $ "component" "engine"|"receiver").
*/}}
{{- define "beekeeper.affinity" -}}
{{- $values := index .root.Values .component }}
{{- if $values.affinity }}
{{- toYaml $values.affinity }}
{{- else -}}
podAntiAffinity:
  preferredDuringSchedulingIgnoredDuringExecution:
    - weight: 100
      podAffinityTerm:
        topologyKey: kubernetes.io/hostname
        labelSelector:
          matchLabels:
            {{- include (printf "beekeeper.%s.selectorLabels" .component) .root | nindent 12 }}
{{- end }}
{{- end }}

{{/*
The PromQL selectors of the engine's and the receiver's series: the namespace
and the job the chart's PodMonitors give them, unless values override them.
*/}}
{{- define "beekeeper.promql.engine" -}}
{{- .Values.prometheusRule.selectors.engine | default (printf "namespace=%q, job=%q" .Release.Namespace (printf "%s/%s" .Release.Namespace (include "beekeeper.engine.name" .))) }}
{{- end }}

{{- define "beekeeper.promql.receiver" -}}
{{- .Values.prometheusRule.selectors.receiver | default (printf "namespace=%q, job=%q" .Release.Namespace (printf "%s/%s" .Release.Namespace (include "beekeeper.receiver.name" .))) }}
{{- end }}
