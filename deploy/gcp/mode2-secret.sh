#!/bin/bash
# Assemble mode 2's Kubernetes Secret: the object the chart's `existingSecret`
# names, which nothing else can build. The chart renders no Secret when
# `existingSecret` is set, and Terraform holds no secret VALUES by design, so
# three keys come from Secret Manager, four are coordinates, and they belong in
# one object only because that is the shape `existingSecret` defines.
#
# Two callers, one source. `.github/workflows/deploy.yml` runs this on every
# push; an operator runs it by hand, and deploy/gcp/README.md shows where its
# inputs come from. They had been two copies of the same seven keys and the same
# three checks, and had already drifted once — the model-providers error routing
# was rewritten in one and not the other (#754).
#
# Inputs, all from the environment so no value ever reaches an argv:
#   PROJECT         the GCP project holding the Secret Manager secrets  (required)
#   BLOB_BUCKET     `terraform output -raw blob_bucket`                 (required)
#   KMS_KEY_NAME    `terraform output -raw kms_key_name`                (required)
#   K8S_NAMESPACE   default `map`
#   K8S_SECRET      default `map-platform`, which is what
#                   deploy/gcp/staging-values.yaml's `existingSecret` names
#   SECRET_DIR      where to leave the fetched values. Default: a fresh mode-700
#                   directory removed on exit. deploy.yml passes one because a
#                   later step reads controlplane-api-key back out of it to
#                   smoke-test the deployment.
#
# It writes the one line `kubectl apply` produced — "created", "configured" or
# "unchanged" — to stdout. That line is the only thing in the deploy that knows
# whether a rotation actually landed, so when `$GITHUB_ENV` is set it is also
# reduced to `SECRET_CHANGED=true|false` there, for the step that rolls the pods.
#
# Reported that way rather than by letting the caller capture stdout, because
# GitHub reads `::add-mask::` off this script's stdout: a caller that captured
# it would swallow every redaction and leave the values unmasked in a public log.
#
# kubectl is driven as `create --dry-run=client -o yaml | kubectl apply -f -`
# rather than `create`: this runs on every push, and `create` fails the second
# time. Apply also makes a rotation a re-run rather than a delete-and-recreate.
set -euo pipefail

# GitHub's log is public, so under Actions every credential is registered as a
# redaction the moment it lands and failures are annotated. Outside Actions both
# are no-ops — an operator's terminal needs neither, and `::error::` in a local
# run is noise that looks like a syntax error.
in_actions() { [ -n "${GITHUB_ACTIONS:-}" ]; }

# Registers a value as a redaction BEFORE anything else can print it. The two
# ways a value gets into the log are both one edit away rather than hypothetical:
# dropping `--out-file` below, which makes gcloud print the value to stdout, and
# a `set -x` added while debugging, which prints every expansion.
#
# What masking does NOT cover is the base64 form — `add-mask` is a literal string
# replacement — and `kubectl create secret -o yaml` emits every value
# base64-encoded. That output stays on the pipe into `kubectl apply` for exactly
# that reason: do not capture it into a variable, and do not `tee` it.
#
# Short lines are left alone deliberately. A mask replaces EVERY occurrence of
# the string in the log, so masking something like "map" would redact half the
# output and hide the failure it was added to catch. Eight characters is the
# shortest value worth protecting; anything shorter is not a credential we can
# usefully hide anyway.
mask_file() {
  in_actions || return 0
  while IFS= read -r line || [ -n "$line" ]; do
    if [ "${#line}" -ge 8 ]; then printf '::add-mask::%s\n' "$line"; fi
  done < "$1"
}

# The headline of a failure: annotated in Actions so it surfaces on the run
# summary, plain on stderr otherwise. The explanation that follows a headline is
# written to stderr directly by the caller, because it is the same text either
# way.
fail() {
  if in_actions; then printf '::error::%s\n' "$1"; else printf '%s\n' "$1" >&2; fi
}

require() {
  if [ -z "${!1:-}" ]; then
    fail "$1 is not set, and this script will not assemble a Secret around an empty value"
    echo "Set it and re-run. deploy/gcp/README.md says where each one comes from." >&2
    exit 1
  fi
}

require PROJECT
require BLOB_BUCKET
require KMS_KEY_NAME

namespace="${K8S_NAMESPACE:-map}"
secret="${K8S_SECRET:-map-platform}"

# Outside any checkout, so no later step can sweep a credential into an image, an
# artifact or a `git add -A`. Mode 700 because the values are written by
# `gcloud --out-file`, which does not chmod what it creates.
#
# A caller-supplied directory is left behind on purpose — deploy.yml's smoke
# step reads a value back out of it — and removed by whoever made it. One the
# script made is its own to clean up, however it exits.
if [ -n "${SECRET_DIR:-}" ]; then
  d="$SECRET_DIR"
  mkdir -p "$d"
else
  d="$(mktemp -d "${TMPDIR:-/tmp}/mode2-secret.XXXXXX")"
  trap 'rm -rf "$d"' EXIT
fi
chmod 700 "$d"

# fetch SECRET_ID DEST — reads the latest version straight to a file.
# `--out-file` rather than a command substitution: the value never becomes a
# shell variable, never reaches an argv, and keeps its exact bytes, which is what
# `--from-file` then hands to Kubernetes.
fetch() {
  if ! gcloud secrets versions access latest \
         --secret="$1" --project="$PROJECT" --out-file="$d/$2"; then
    fail "cannot read Secret Manager secret '$1' in $PROJECT"
    echo "The mode-2 deploy needs it. Check it exists and has an ENABLED version:" >&2
    echo "  gcloud secrets versions list $1 --project=$PROJECT" >&2
    return 1
  fi
  # A blank value is the failure worth catching here rather than at pod start: it
  # is what a failed generator piped into `gcloud secrets versions add` produces
  # — stored successfully, and now `latest`. The Secret would apply cleanly and
  # the platform would then authenticate as nobody.
  #
  # Blank rather than zero-byte, because a lone space is neither empty nor a
  # newline and so passes both a `test -s` and the newline check below, while
  # being no more usable than nothing: the api key is compared verbatim against a
  # header the parser has already trimmed, and the DSN parses as no connection
  # settings at all, which sends the pool to a localhost that is not there.
  if [ -z "$(tr -d '[:space:]' < "$d/$2")" ]; then
    fail "Secret Manager secret '$1' has an EMPTY or whitespace-only latest version"
    echo "Add a real version and disable that one." >&2
    return 1
  fi
  mask_file "$d/$2"
}

fetch controlplane-api-key controlplane-api-key
fetch database-url         database-url

# A trailing newline is the classic way one of these gets stored (an `echo` where
# bootstrap.sh uses `printf '%s'`), and both are embedded VERBATIM: one in an
# x-api-key comparison, one as a DSN. So the deploy would succeed and the
# platform would then reject the very key CD smoke-tests with, or fail to parse
# its own database URL. Refuse it here, where the fix is one command.
#
# model-providers.json is deliberately NOT checked this way: it is a JSON
# document read by a parser, a human adds it with `--data-file=/path/to/file`,
# and every editor on earth ends a file with a newline. Its shape is checked
# below instead.
for f in controlplane-api-key database-url; do
  if [ "$(wc -l < "$d/$f")" -ne 0 ]; then
    fail "Secret Manager secret '$f' contains a newline"
    echo "It is used verbatim, so a trailing newline is part of the value." >&2
    echo "Re-add it without one: printf '%s' \"\$value\" | gcloud secrets" >&2
    echo "versions add $f --project=$PROJECT --data-file=-" >&2
    exit 1
  fi
done

# model-providers is fetched on its own because its failure mode is its own.
# Nothing in this repository creates or fills it — deploy/gcp/README.md
# ("Continuous delivery") owns which secrets are stood up out of band and by whom
# — so it may be missing, or present with no version, and those need different
# commands. This script cannot tell which: for a secret that is absent,
# `versions access latest` answers `NOT_FOUND: Secret [...] not found or has no
# versions` — measured against the API at gcloud 578, a sentence that names both
# states while committing to neither. Whatever a versionless one answers, that
# reply already leaves the state open, so one branch handles both and names both
# commands, create first. What the secret holds when it is right is a live model
# API key, the one credential no automation may mint, so this script can repair
# neither state: it stops rather than deploying a brain that crash-loops on an
# empty config.
if ! gcloud secrets versions access latest \
       --secret=model-providers --project="$PROJECT" \
       --out-file="$d/model-providers.json" 2>"$d/model-providers.err"; then
  rc=1
else
  rc=0
fi
# Replayed either way, so a warning on the success path is not swallowed by the
# capture that exists for the failure path.
cat "$d/model-providers.err" >&2
if [ "$rc" -ne 0 ]; then
  # This branch catches every way the read can fail, and the advice below answers
  # exactly one of them. A denied permission, a disabled API, a version someone
  # disabled or an unreachable endpoint is not a secret anybody needs to create,
  # so route on the status Google returned and let the error above speak for the
  # rest.
  if ! grep -q NOT_FOUND "$d/model-providers.err"; then
    fail "cannot read the 'model-providers' secret — the error above says why, and it is not a NOT_FOUND"
    echo "The error above is the whole of what this script knows, and it does not say" >&2
    echo "the secret is absent — though a caller without access is told that too," >&2
    echo "so absent is not ruled out either. Start with what it does name: the" >&2
    echo "deploy identity's Secret Manager access, whether the API is enabled on" >&2
    echo "$PROJECT, and whether the newest version was disabled." >&2
    exit 1
  fi
  fail "cannot read the 'model-providers' secret — it must exist AND have an enabled version, and only a human can supply one"
  cat >&2 <<EOF
This is deliberate and this script cannot fix it: model-providers holds a live
model API key.

Nothing in this repository creates the secret, so on a fresh environment it does
not exist at all and this comes first. Skip it if it does exist:

  gcloud secrets create model-providers \\
    --project=$PROJECT --replication-policy=automatic

Then write the routes to a file and add them as its version:

  [ { "model": "*", "protocol": "anthropic",
      "base_url": "https://your-gateway.example",
      "api_key": "sk-..." } ]

  gcloud secrets versions add model-providers \\
    --project=$PROJECT --data-file=/path/to/model-providers.json

The accepted keys are documented under brain.modelProviders in
deploy/helm/managed-agent-platform/values.yaml. base_url is the API ROOT: the
adapter appends /v1/messages or /v1/chat/completions itself, so omit a trailing
/v1.
EOF
  exit 1
fi

# The brain's loader (internal/provider.LoadRoutes) requires a JSON ARRAY of
# route objects and rejects anything else with "must be a JSON array". Checking
# it here costs a second; getting it wrong costs the full `--wait --atomic
# --timeout 10m` before the release rolls back.
#
# `-s` is what makes it one document. Without it jq evaluates each top-level
# document in turn and `-e` takes its status from the LAST one, so a bare object
# followed by a valid array passes while the bytes Kubernetes stores are not a
# JSON array at all. A second `versions add` cannot produce that — it writes a
# whole new version — but one `--data-file` holding two documents can, which is
# what concatenating two route files, or editing one in place, produces.
if ! jq -e -s 'length == 1 and (.[0] | type == "array" and length > 0 and all(.[]; has("model")))' \
     "$d/model-providers.json" > /dev/null; then
  fail "the 'model-providers' secret is not a non-empty JSON array of route objects"
  echo "Each entry needs at least \"model\" (\"*\" is the default route)." >&2
  exit 1
fi

# The api_key inside is the actual secret; the surrounding JSON is not. Masking
# per line would be useless here (a pretty-printed payload would register "[" as
# a redaction), so mask exactly the keys.
if in_actions; then
  jq -r '.[].api_key // empty' "$d/model-providers.json" |
    while IFS= read -r k; do
      if [ "${#k}" -ge 8 ]; then printf '::add-mask::%s\n' "$k"; fi
    done
fi

# The four coordinates. Non-secret, but the chart reads all seven keys from this
# one object — `blob-backend` missing does not fail, it reads as the default "s3"
# and then looks for an endpoint that is not there.
printf '%s' gcs              > "$d/blob-backend"
printf '%s' "$BLOB_BUCKET"   > "$d/blob-bucket"
printf '%s' gcpkms           > "$d/secrets-backend"
printf '%s' "$KMS_KEY_NAME"  > "$d/gcpkms-key-name"

# The namespace must exist before the Secret does, and `helm --create-namespace`
# runs after this. Applied the same way for the same reason: it is already there
# on every run but the first.
kubectl create namespace "$namespace" --dry-run=client -o yaml \
  | kubectl apply -f - > /dev/null

# --from-file, never --from-literal. A literal would put every one of these on
# this process's argv, visible in `ps` to anything else on the machine and in any
# log that captured the command line. The file name is the Secret key, which is
# why the fetch destinations above are named after the keys rather than after the
# Secret Manager secrets.
#
# Its one line of output — "created", "configured" or "unchanged" — is what this
# script exists to report: it is the only thing that knows whether a rotation
# actually landed. It names the object and never a value, so it is safe to hold
# in a variable, unlike the manifest on the pipe.
applied="$(kubectl create secret generic "$secret" \
  --namespace "$namespace" \
  --from-file="$d/controlplane-api-key" \
  --from-file="$d/database-url" \
  --from-file="$d/model-providers.json" \
  --from-file="$d/blob-backend" \
  --from-file="$d/blob-bucket" \
  --from-file="$d/secrets-backend" \
  --from-file="$d/gcpkms-key-name" \
  --dry-run=client -o yaml \
  | kubectl apply -f -)"
printf '%s\n' "$applied"

# Nothing downstream can notice a rotation on its own. Env from a `secretKeyRef`
# is read once, at process start; `helm upgrade` cannot see this Secret at all,
# because `existingSecret` means the chart renders none and its
# `checksum/secret` annotation therefore hashes an empty template on every run;
# and the case this matters in — a run after a human rotated a Secret Manager
# value — deploys the same commit, so the pod template is byte-identical and
# Kubernetes correctly does nothing. The pods would go on serving the archived
# key and the old DSN. So record whether the Secret changed; deploy.yml's step
# after the upgrade rolls the pods when it did.
if [ -n "${GITHUB_ENV:-}" ]; then
  case "$applied" in
    *unchanged) echo "SECRET_CHANGED=false" >> "$GITHUB_ENV" ;;
    *) echo "SECRET_CHANGED=true" >> "$GITHUB_ENV" ;;
  esac
fi
