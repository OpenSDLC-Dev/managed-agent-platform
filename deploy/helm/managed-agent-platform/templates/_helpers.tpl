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
The BLOB_* env entries for processes that reach object storage (the
controlplane; the executor joined with skills materialization; the brain
joined with plan 21's file-rubric snapshots). Every key is optional: a chart
Secret rendered without blob-* keys — or an existingSecret that never carried
them — deploys the platform without object storage, and the processes serve
with skills (and file-rubric grading) unavailable instead of crash-looping.
*/}}
{{- define "map.blobEnv" -}}
{{- range $var, $key := dict "BLOB_BACKEND" "blob-backend" "BLOB_ENDPOINT" "blob-endpoint" "BLOB_ACCESS_KEY" "blob-access-key" "BLOB_SECRET_KEY" "blob-secret-key" "BLOB_BUCKET" "blob-bucket" "BLOB_REGION" "blob-region" "BLOB_TLS" "blob-tls" "BLOB_BUCKET_PRECREATED" "blob-bucket-precreated" }}
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
{{- range $var, $key := dict "SECRETS_BACKEND" "secrets-backend" "BAO_ADDR" "bao-addr" "BAO_TOKEN" "bao-token" "BAO_TRANSIT_KEY" "bao-transit-key" "SECRETS_MASTER_KEY" "secrets-master-key" "SECRETS_KEY_ID" "secrets-key-id" "GCPKMS_KEY_NAME" "gcpkms-key-name" }}
- name: {{ $var }}
  valueFrom:
    secretKeyRef:
      name: {{ include "map.secretName" $ }}
      key: {{ $key }}
      optional: true
{{- end }}
{{- end -}}

{{/*
The IDENTITY_* env entries for the control plane — the human-auth lane
(docs/plan/31_console-sso-rbac.md, #56). Rendered inside the controlplane
container's `env:` list, like map.commonEnv above. No other process has a human
lane, so no other Deployment includes this.

Emits NOTHING while identity.mode is empty, and that is not a shortcut around a
default: `IDENTITY_MODE` unset and `IDENTITY_MODE=disabled` are one state to
internal/identity's ConfigFromEnv, which returns immediately and reads none of the
other variables. Rendering the pair explicitly would therefore add an env entry to
every existing release without changing any behaviour — and roll its control-plane
pods on upgrade to do it.

Each remaining variable is emitted only when set, so the platform's own defaults
(the `roles`/`email`/`name` claim names, the five-algorithm allowlist) come from
the binary rather than being restated here in a second place they could drift.
*/}}
{{- define "map.identityEnv" -}}
{{- $identity := .Values.identity -}}
{{- if $identity.mode }}
- name: IDENTITY_MODE
  value: {{ $identity.mode | quote }}
{{- /* A dict, like map.blobEnv's: the twelve are uniform, and `range` over a map
       is sorted by key, so a given values file always renders the same list. */}}
{{- range $var, $value := dict
      "IDENTITY_OIDC_ISSUER" $identity.oidc.issuer
      "IDENTITY_OIDC_AUDIENCE" $identity.oidc.audience
      "IDENTITY_OIDC_JWKS_URL" $identity.oidc.jwksURL
      "IDENTITY_PROXY_PRESET" $identity.proxy.preset
      "IDENTITY_PROXY_HEADER" $identity.proxy.header
      "IDENTITY_PROXY_ISSUER" $identity.proxy.issuer
      "IDENTITY_PROXY_AUDIENCE" $identity.proxy.audience
      "IDENTITY_PROXY_KEYS_URL" $identity.proxy.keysURL
      "IDENTITY_PROXY_ALGS" $identity.proxy.algs
      "IDENTITY_CLAIM_ROLES" $identity.claims.roles
      "IDENTITY_CLAIM_EMAIL" $identity.claims.email
      "IDENTITY_CLAIM_NAME" $identity.claims.name }}
{{- if $value }}
- name: {{ $var }}
  value: {{ $value | quote }}
{{- end }}
{{- end }}
{{- /* The role map is written as a map — the shape it is — and encoded here into
       the flat `value=role,value=role` the verifier parses, so nobody hand-writes
       an env-var dialect (the same trade sandboxPlacement makes). A YAML map also
       makes a duplicate source value impossible to express, which is the one
       defect the verifier calls out as a silent authority change.

       `,` and `=` are this encoding's separators, so a claim value or a role
       carrying one would render as a DIFFERENT, still-valid map that the verifier
       accepts without complaint. Neither is legal in a role name, and a group name
       containing one cannot be configured through IDENTITY_ROLE_MAP at all, so
       refusing here costs nothing and turns the encoding's one silent failure into
       a render error. Non-strings are refused for the reason the node selector
       refuses them: Helm decodes an unquoted value in a file as float64, which
       renders as something no role name ever is. */}}
{{- /* An empty map is not a valid deployment of this lane, and it is the one
       omission whose consequence is not a denied human but a dead control plane:
       parseRoleMap answers "IDENTITY_ROLE_MAP is required and must map at least
       one claim value", FromEnv propagates it, and the process exits — so the
       machine lanes go down with the human one. The chart refuses the other
       values whose absence is fatal; this one is fatal to more than itself. */}}
{{- if not $identity.roleMap }}
{{- fail "identity.roleMap must map at least one claim value when identity.mode is set: the control plane refuses to start without it, so an empty map does not merely deny humans, it takes the whole process down at boot — machine lanes included." }}
{{- end }}
{{- $pairs := list }}
{{- range $value, $role := $identity.roleMap }}
{{- if not (kindIs "string" $role) }}
{{- fail (printf "identity.roleMap[%s]: a role must be one of the quoted strings admin, developer or viewer (got %s)" $value (kindOf $role)) }}
{{- end }}
{{- if or (contains "," $value) (contains "=" $value) (contains "," $role) (contains "=" $role) }}
{{- fail (printf "identity.roleMap[%s]=%s: neither a claim value nor a role may contain ',' or '=' — they separate entries in IDENTITY_ROLE_MAP" $value $role) }}
{{- end }}
{{- $pairs = append $pairs (printf "%s=%s" $value $role) }}
{{- end }}
{{- with $pairs }}
- name: IDENTITY_ROLE_MAP
  value: {{ join "," . | quote }}
{{- end }}
{{- end }}
{{- end -}}

{{/*
The bundled IdP's external URL: `https://` + the ingress host, and nothing else.

It is one value with two jobs, which is why it is computed in one place. Casdoor
takes it as `origin` — the base of every redirect its login page issues, and the
`iss` claim of every token it mints — and the verifier compares `iss` exactly, so
identity.oidc.issuer has to equal this string byte for byte. The scheme is not a
knob: the platform refuses a plain-HTTP issuer and key set, so an http origin here
would be an IdP this control plane could not be wired to.
*/}}
{{- define "map.casdoorOrigin" -}}
{{- if not .Values.casdoor.ingress.host -}}
{{- fail "casdoor.ingress.host is required when casdoor.enabled: it is the address the browser reaches the IdP on, and Casdoor stamps it into every token as the `iss` claim." -}}
{{- end -}}
{{- printf "https://%s" .Values.casdoor.ingress.host -}}
{{- end -}}

{{/*
The Cloud SQL Auth Proxy sidecar (#269), rendered under the podSpec of each
process that opens the database — the controlplane, the brain and the executor.
Emits nothing at all unless cloudSQLProxy.enabled. Call with the root context at
the podSpec's own indentation.

A NATIVE sidecar — an initContainer with `restartPolicy: Always` — which is the
shape Google documents for GKE, and here it is load-bearing rather than modern:
all three processes open the database as they start (store.Open migrates), so an
ordinary container would let them race the proxy and crash-loop until it caught
up. A native sidecar with a startupProbe starts first and is *ready* first, and
is torn down last.

Not `--auto-iam-authn`, which Google's example carries: that selects IAM database
authentication, and the platform authenticates as an ordinary PostgreSQL role
whose password rides the DSN — the role deploy/gcp/dbinit.sql creates, precisely
so it is not a Cloud SQL Admin API user.

The guards live here rather than in secret.yaml, where the chart's other
mutually-exclusive-value refusals live, because everything past that file's
first three refusals sits inside its `if not .Values.existingSecret` block and
is skipped when existingSecret is set — and existingSecret is exactly how the
GCP mode this sidecar exists for is deployed, so a guard written in that block
would never fire for the deployments that need it.
*/}}
{{- define "map.cloudSQLProxy" -}}
{{- if .Values.cloudSQLProxy.enabled }}
{{- if .Values.postgresql.enabled }}
{{- fail "cloudSQLProxy.enabled is incompatible with postgresql.enabled: the bundled Postgres is the database in that mode, and one `database-url` cannot name both it and the proxy's loopback socket. Set postgresql.enabled=false." }}
{{- end }}
{{- if not .Values.cloudSQLProxy.instanceConnectionName }}
{{- fail "cloudSQLProxy.instanceConnectionName is required when cloudSQLProxy.enabled: PROJECT:REGION:INSTANCE, from `terraform output -raw sql_instance_connection_name`." }}
{{- end }}
{{- /* Three colon-separated segments, or four for a legacy domain-scoped
       project (`google.com:project:region:instance`) — and every one of them
       non-empty. That catches most of what a bad substitution produces: a bare
       address (`10.1.2.3`), a bare word, and an empty part (`a::b`, `p:r:`).

       It does NOT catch an address WITH a port. `127.0.0.1:5432:x` is three
       non-empty segments and passes — this is a shape filter, not a backstop,
       and the distinction matters because nothing downstream catches it
       either: the proxy dials only under `--run-connection-test`, which this
       chart does not pass, so the pod goes ready on a name that names nothing.
       Same for a well-formed name whose project or instance is simply wrong.
       #493 is that gap.

       Checking the SHAPE and stopping there is deliberate. Google's own rules
       for the three parts are narrower, and encoding them here would refuse a
       valid name the day Google widens one — the proxy is the authority on its
       own argument. */}}
{{- $segments := splitList ":" .Values.cloudSQLProxy.instanceConnectionName }}
{{- $empty := false }}
{{- range $segments }}{{- if eq . "" }}{{- $empty = true }}{{- end }}{{- end }}
{{- if or $empty (lt (len $segments) 3) (gt (len $segments) 4) }}
{{- fail (printf "cloudSQLProxy.instanceConnectionName must be an instance connection name — PROJECT:REGION:INSTANCE, or DOMAIN:PROJECT:REGION:INSTANCE for a domain-scoped project, with no empty part. Not %q. The proxy is given a name, never an address." .Values.cloudSQLProxy.instanceConnectionName) }}
{{- end }}
{{- if semverCompare "<1.29-0" .Capabilities.KubeVersion.Version }}
{{- fail "cloudSQLProxy.enabled needs Kubernetes >= 1.29: the proxy runs as a native sidecar (an initContainer with restartPolicy Always), which older kubelets reject" }}
{{- end }}
initContainers:
  - name: cloud-sql-proxy
    image: {{ .Values.cloudSQLProxy.image | quote }}
    restartPolicy: Always
    args:
      - "--structured-logs"
      {{- /* 127.0.0.1 is the proxy's own default address and is not restated as
             a flag, because it is the whole safety argument for the
             `sslmode=disable` this topology asks for: bind it anywhere else and
             the loopback claim stops being true. */}}
      - "--port=5432"
      {{- if .Values.cloudSQLProxy.privateIP }}
      - "--private-ip"
      {{- end }}
      {{- /* The health endpoints are what make the startupProbe below possible;
             they must bind 0.0.0.0 because the kubelet probes the pod's address,
             not its loopback. They serve `/startup`, `/liveness` and
             `/readiness` and nothing else — no database traffic crosses them. */}}
      - "--health-check"
      - "--http-address=0.0.0.0"
      - "--http-port=9801"
      - {{ .Values.cloudSQLProxy.instanceConnectionName | quote }}
    {{- /* A native sidecar without a startupProbe only promises that the
           process was STARTED before the application container, which is not
           the promise that matters — the listener is what the application needs.
           With one, the kubelet holds the application back until `/startup`
           answers 200. */}}
    startupProbe:
      httpGet:
        path: /startup
        port: 9801
      periodSeconds: 1
      failureThreshold: 60
    livenessProbe:
      httpGet:
        path: /liveness
        port: 9801
      periodSeconds: 10
      failureThreshold: 3
    securityContext:
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities:
        drop: ["ALL"]
    {{- with .Values.cloudSQLProxy.resources }}
    resources:
      {{- toYaml . | nindent 6 }}
    {{- end }}
{{- end }}
{{- end -}}

{{/* imagePullSecrets block, rendered under a podSpec. */}}
{{- define "map.imagePullSecrets" -}}
{{- with .Values.imagePullSecrets }}
imagePullSecrets:
{{- toYaml . | nindent 0 }}
{{- end }}
{{- end -}}
