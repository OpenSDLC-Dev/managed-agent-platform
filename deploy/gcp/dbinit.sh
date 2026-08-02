#!/usr/bin/env bash
# Creates the platform's database role outside cloudsqlsuperuser, from inside
# the cluster, and fails if the result is not what deploy/gcp/dbinit.sql asserts.
#
# WHY A JOB AND NOT A LOCAL psql. environment/ puts Cloud SQL on a private
# address (ipv4_enabled = false), so the instance is reachable from the VPC and
# from nowhere else — including not from the machine running this script. A pod
# is the shortest thing that is inside the VPC. It connects straight to the
# private IP with sslmode=require rather than through the Cloud SQL Auth Proxy,
# which means this run also measures whether a Pod can reach a private-IP
# instance directly; dbinit.sql asserts the session is encrypted, so a
# connection that silently fell back to plaintext fails rather than passes.
#
# Run order:
#     make gcp-foundation-apply
#     make gcp-bootstrap             # the two database passwords get values
#     make gcp-env-apply             # instance, database, `postgres` admin
#     make gcp-db-init               # this script
#     helm install ...               # the platform, as the role created here
#
# Idempotent. Re-run it after a rebuild of environment/, and after bootstrap.sh
# rotates the password — the second case is the whole rotation procedure.
#
# Nothing here puts a secret in argv. `ps` is readable by every process on the
# machine, and `kubectl create secret --from-literal` would put the value there;
# both passwords travel through mode-600 files in a private temp directory that
# is removed on every exit path.

set -euo pipefail

PROJECT="${PROJECT:-}"
NAME_PREFIX="${NAME_PREFIX:-map}"
NAMESPACE="${NAMESPACE:-map}"
# Pulled by the cluster, so it must be reachable from a PRIVATE node: Docker Hub
# through Cloud NAT works, and so does the environment's own mirror. Override
# with the mirror prefix to avoid depending on an anonymous rate limit —
# `terraform -chdir=deploy/gcp/environment output -raw docker_hub_mirror` gives
# the prefix, and the image is then PREFIX/library/postgres:16-alpine.
DBINIT_IMAGE="${DBINIT_IMAGE:-postgres:16-alpine}"
ENV_DIR="${ENV_DIR:-deploy/gcp/environment}"

if [[ -z "$PROJECT" ]]; then
	echo "PROJECT is required: PROJECT=my-project make gcp-db-init" >&2
	exit 2
fi

for tool in gcloud kubectl terraform; do
	command -v "$tool" >/dev/null || { echo "$tool is required and not on PATH" >&2; exit 2; }
done

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
sql="$here/dbinit.sql"
[[ -r "$sql" ]] || { echo "cannot read $sql" >&2; exit 2; }

# ---------------------------------------------------------------------------
# What to connect to. Read from Terraform rather than reconstructed, so a
# renamed instance or a re-created one cannot leave this script pointed at a
# stale address.
# ---------------------------------------------------------------------------
tf_out() {
	local v
	if ! v="$(terraform -chdir="$ENV_DIR" output -raw "$1" 2>&1)"; then
		echo "could not read terraform output '$1' from $ENV_DIR:" >&2
		echo "$v" >&2
		echo "Run 'make gcp-env-apply' first." >&2
		exit 1
	fi
	[[ -n "$v" ]] || { echo "terraform output '$1' is empty" >&2; exit 1; }
	printf '%s' "$v"
}

db_host="$(tf_out sql_private_ip)"
db_name="$(tf_out sql_database)"
db_user="$(tf_out sql_user)"
db_admin="$(tf_out sql_admin_user)"
app_role="$(tf_out sql_app_role)"

# ---------------------------------------------------------------------------
# The credentials. Read into a private temp directory rather than into shell
# variables that a later `set -x` or an error trap could print.
# ---------------------------------------------------------------------------
workdir="$(mktemp -d)"
chmod 700 "$workdir"

job="${NAME_PREFIX}-dbinit"

# Set only on the timeout path. A Job that never reached a terminal condition is
# the one case where the pod itself is the evidence — deleting it would take the
# events, the pull error and the pending reason with it.
keep_job=0

cleanup() {
	# Runs on every exit path, including the failure ones. Each delete is allowed
	# to fail — the Job may never have been created — but the credentials must go
	# regardless, so the Secret and the temp directory are removed even when the
	# Job is deliberately kept.
	set +e
	if [[ "$keep_job" -eq 0 ]]; then
		kubectl -n "$NAMESPACE" delete job "$job" --ignore-not-found >/dev/null 2>&1
		kubectl -n "$NAMESPACE" delete configmap "$job" --ignore-not-found >/dev/null 2>&1
	fi
	kubectl -n "$NAMESPACE" delete secret "$job" --ignore-not-found >/dev/null 2>&1
	rm -rf "$workdir"
}
trap cleanup EXIT

read_secret() {
	local secret="$1" dest="$2"
	# Written straight to a file the shell created with a private umask, so the
	# value never becomes a shell variable and never reaches a process argument
	# list. `gcloud ... access` writes the payload verbatim with no trailing
	# newline, which is what the secret holds and what the database must be given.
	if ! (umask 077 && gcloud secrets versions access latest \
		--secret="$secret" --project "$PROJECT" >"$dest" 2>"$workdir/err"); then
		echo "could not read secret $secret:" >&2
		cat "$workdir/err" >&2
		echo "Run 'make gcp-bootstrap' first." >&2
		exit 1
	fi
	if [[ ! -s "$dest" ]]; then
		echo "secret $secret is empty — refusing to configure an empty password" >&2
		exit 1
	fi
	# A trailing newline would be stored as part of the password and would then
	# be missing from every DSN that quotes the secret's value, which fails as an
	# authentication error with nothing to point at. bootstrap.sh strips it; a
	# hand-written version may not have.
	if [[ "$(tail -c 1 "$dest" | od -An -c | tr -d ' ')" == '\n' ]]; then
		echo "secret $secret ends in a newline, which would become part of the password." >&2
		echo "Store it without one: printf '%s' VALUE | gcloud secrets versions add ..." >&2
		exit 1
	fi
}

read_secret "${NAME_PREFIX}-db-admin-password" "$workdir/admin-password"
read_secret "${NAME_PREFIX}-db-password" "$workdir/db-password"

# ---------------------------------------------------------------------------
# Ship it. The SQL goes in a ConfigMap because it is not secret; the two
# passwords go in a Secret because they are, and --from-file is what keeps them
# out of argv.
# ---------------------------------------------------------------------------

kubectl create namespace "$NAMESPACE" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

# Deleted first rather than applied over: a Job's pod template is immutable, so
# a second run against a surviving Job fails with a field-is-immutable error
# that says nothing about what to do.
kubectl -n "$NAMESPACE" delete job "$job" --ignore-not-found >/dev/null
kubectl -n "$NAMESPACE" delete secret "$job" --ignore-not-found >/dev/null
kubectl -n "$NAMESPACE" delete configmap "$job" --ignore-not-found >/dev/null

kubectl -n "$NAMESPACE" create configmap "$job" --from-file=dbinit.sql="$sql" >/dev/null
kubectl -n "$NAMESPACE" create secret generic "$job" \
	--from-file=admin-password="$workdir/admin-password" \
	--from-file=db-password="$workdir/db-password" >/dev/null

kubectl -n "$NAMESPACE" apply -f - >/dev/null <<YAML
apiVersion: batch/v1
kind: Job
metadata:
  name: ${job}
spec:
  # A failure is a failure. The default of six retries would run the assertions
  # six times and report the last one, which turns "this privilege was never
  # narrowed" into six identical failures and a long wait.
  backoffLimit: 0
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: psql
          image: ${DBINIT_IMAGE}
          command:
            - psql
            - --no-psqlrc
            - -v
            - db_user=${db_user}
            - -v
            - app_role=${app_role}
            - -v
            - db_name=${db_name}
            - -f
            - /sql/dbinit.sql
          env:
            - name: PGHOST
              value: "${db_host}"
            - name: PGPORT
              value: "5432"
            - name: PGDATABASE
              value: "${db_name}"
            - name: PGUSER
              value: "${db_admin}"
            # Encryption demanded by the client AND asserted server-side inside
            # dbinit.sql. Asking for it here is what makes a plaintext fallback
            # a connection error rather than a silent downgrade.
            - name: PGSSLMODE
              value: require
            - name: PGPASSWORD
              valueFrom:
                secretKeyRef:
                  name: ${job}
                  key: admin-password
            - name: MAP_DB_PASSWORD
              valueFrom:
                secretKeyRef:
                  name: ${job}
                  key: db-password
          volumeMounts:
            - name: sql
              mountPath: /sql
              readOnly: true
      volumes:
        - name: sql
          configMap:
            name: ${job}
YAML

echo "running ${job} in namespace ${NAMESPACE} against ${db_host}"

# Wait for EITHER outcome. Waiting only for complete means a failed Job is not
# observed until the timeout expires, and the operator is told "timed out"
# about a run that failed in four seconds with a precise message.
deadline=$((SECONDS + 300))
state=""
while ((SECONDS < deadline)); do
	if [[ "$(kubectl -n "$NAMESPACE" get job "$job" \
		-o 'jsonpath={.status.conditions[?(@.type=="Complete")].status}' 2>/dev/null)" == "True" ]]; then
		state=complete
		break
	fi
	if [[ "$(kubectl -n "$NAMESPACE" get job "$job" \
		-o 'jsonpath={.status.conditions[?(@.type=="Failed")].status}' 2>/dev/null)" == "True" ]]; then
		state=failed
		break
	fi
	sleep 3
done

echo "--- ${job} log ---"
# Never allowed to sink the script: an empty log from a pod that was evicted
# before it started is a diagnosis, not a reason to lose the exit status below.
kubectl -n "$NAMESPACE" logs "job/$job" --tail=-1 2>&1 || true
echo "--- end ---"

case "$state" in
complete)
	echo "ok: ${db_user} exists, is outside cloudsqlsuperuser, and owns ${db_name}"
	;;
failed)
	echo "FAILED: dbinit did not complete — see the log above" >&2
	exit 1
	;;
*)
	# Kept, and the Secret is still deleted — so what remains is a Job that can
	# be described and a pod whose events explain the wait, with no credential
	# left behind in the cluster.
	keep_job=1
	echo "TIMED OUT after 300s waiting for ${job} to reach a terminal state." >&2
	echo "The Job is LEFT IN PLACE so it can be inspected (its Secret is not):" >&2
	echo "  kubectl -n ${NAMESPACE} describe job ${job}" >&2
	echo "  kubectl -n ${NAMESPACE} get pods -l job-name=${job}" >&2
	echo "Delete it with: kubectl -n ${NAMESPACE} delete job ${job} configmap/${job}" >&2
	exit 1
	;;
esac
