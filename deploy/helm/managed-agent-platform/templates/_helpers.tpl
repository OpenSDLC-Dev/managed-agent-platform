{{/*
Chart name, release-qualified fullname, and the label sets. Standard Helm idioms.
*/}}
{{- define "map.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Truncated to 50, not the usual 63: every resource appends a component suffix (the
longest is "-controlplane", 13 chars), and the result must stay within the 63-char
DNS label limit for Service names. 50 + 13 = 63. Helm caps release names at 53, so
a long release name would otherwise render an invalid > 63-char Service name.
*/}}
{{- define "map.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 50 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "map.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/* Labels shared by every object. */}}
{{- define "map.labels" -}}
helm.sh/chart: {{ include "map.chart" . }}
app.kubernetes.io/name: {{ include "map.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
app.kubernetes.io/part-of: managed-agent-platform
{{- end -}}

{{/*
Per-component selector labels. Call with a dict: (dict "ctx" . "component" "brain").
Selector labels are immutable on a Deployment, so keep this set minimal and stable.
*/}}
{{- define "map.selectorLabels" -}}
app.kubernetes.io/name: {{ include "map.name" .ctx }}
app.kubernetes.io/instance: {{ .ctx.Release.Name }}
app.kubernetes.io/component: {{ .component }}
{{- end -}}

{{/* Name of the Secret every process reads credentials from. */}}
{{- define "map.secretName" -}}
{{- if .Values.existingSecret -}}
{{- .Values.existingSecret -}}
{{- else -}}
{{- include "map.fullname" . -}}
{{- end -}}
{{- end -}}

{{/*
Image reference for a component. Call with (dict "ctx" . "component" "controlplane").
{registry}/{repository}/{component}:{tag}, tag defaulting to the chart appVersion.
*/}}
{{- define "map.image" -}}
{{- $img := .ctx.Values.image -}}
{{- $tag := default .ctx.Chart.AppVersion $img.tag -}}
{{- printf "%s/%s/%s:%s" $img.registry $img.repository .component $tag -}}
{{- end -}}

{{/*
The env entries every process shares: DATABASE_URL and the OTLP wiring. Rendered
inside a container's `env:` list. Call with the root context.
*/}}
{{- define "map.commonEnv" -}}
- name: DATABASE_URL
  valueFrom:
    secretKeyRef:
      name: {{ include "map.secretName" . }}
      key: database-url
{{- if .Values.otlp.endpoint }}
- name: OTEL_EXPORTER_OTLP_ENDPOINT
  value: {{ .Values.otlp.endpoint | quote }}
- name: OTEL_EXPORTER_OTLP_INSECURE
  value: {{ .Values.otlp.insecure | quote }}
{{- end }}
{{- end -}}

{{/*
The BLOB_* env entries for processes that reach object storage (today the
controlplane; the executor joins with skills materialization). Every key is
optional: a chart Secret rendered without blob-* keys — or an existingSecret
that never carried them — deploys the platform without object storage, and
the controlplane serves with skills unavailable instead of crash-looping.
*/}}
{{- define "map.blobEnv" -}}
{{- range $var, $key := dict "BLOB_ENDPOINT" "blob-endpoint" "BLOB_ACCESS_KEY" "blob-access-key" "BLOB_SECRET_KEY" "blob-secret-key" "BLOB_BUCKET" "blob-bucket" "BLOB_REGION" "blob-region" "BLOB_TLS" "blob-tls" }}
- name: {{ $var }}
  valueFrom:
    secretKeyRef:
      name: {{ include "map.secretName" $ }}
      key: {{ $key }}
      optional: true
{{- end }}
{{- end -}}

{{/*
The SECRETS_ and BAO_ env entries for processes that use the credential cipher
(docs/plan/12_vaults-credentials.md: the controlplane encrypts on write and
decrypts for mcp_oauth_validate; the executor decrypts at egress substitution;
the brain joins with #45; the BYOC worker never talks to bao). Every key is
optional, exactly like map.blobEnv: a chart Secret rendered without secrets-*
keys — or an existingSecret that never carried them — deploys without a cipher,
and the processes serve with vault credential storage unavailable instead of
crash-looping.
*/}}
{{- define "map.secretsEnv" -}}
{{- range $var, $key := dict "SECRETS_BACKEND" "secrets-backend" "BAO_ADDR" "bao-addr" "BAO_TOKEN" "bao-token" "BAO_TRANSIT_KEY" "bao-transit-key" "SECRETS_MASTER_KEY" "secrets-master-key" "SECRETS_KEY_ID" "secrets-key-id" }}
- name: {{ $var }}
  valueFrom:
    secretKeyRef:
      name: {{ include "map.secretName" $ }}
      key: {{ $key }}
      optional: true
{{- end }}
{{- end -}}

{{/* imagePullSecrets block, rendered under a podSpec. */}}
{{- define "map.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end -}}
