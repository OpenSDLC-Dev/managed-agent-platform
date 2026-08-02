#!/usr/bin/env bash
# Adds the first version of each Secret Manager secret the foundation created,
# and creates the GCS HMAC key whose secret those secrets hold.
#
# This exists because Terraform must not (plan 20, Decision 6). A value in
# `secret_data` is a value in state, in plaintext, in both configurations —
# including the one that is destroyed and rebuilt routinely. So Terraform owns
# names, IAM bindings and preconditions, and value creation lives here.
#
# Run order:
#     make gcp-foundation-apply     # the secrets exist, empty
#     make gcp-bootstrap            # this script: they get values
#     make gcp-env-apply            # reads them, never storing one
#
# Idempotent by SKIPPING, not by overwriting. The acceptance criterion is
# reproduce-from-clean, not reproduce-by-overwrite: a secret that already has a
# version is left exactly as it is, because that version may be the only thing
# that can decrypt something. Re-running after an `environment/` teardown is the
# normal case and must be a no-op.
#
# Nothing here ever puts a secret in argv. `ps` is readable by every process on
# the machine and shell history outlives the session, so every value travels on
# stdin via --data-file=-.

set -euo pipefail

PROJECT="${PROJECT:-}"
NAME_PREFIX="${NAME_PREFIX:-map}"

if [[ -z "$PROJECT" ]]; then
	echo "PROJECT is required: PROJECT=my-project make gcp-bootstrap" >&2
	exit 2
fi

# python3 is in this list because it parses the HMAC create response. A missing
# python3 discovered AFTER that create would leave an orphaned key whose secret
# is already lost — see the rollback trap below.
for tool in gcloud openssl python3; do
	command -v "$tool" >/dev/null || { echo "$tool is required and not on PATH" >&2; exit 2; }
done

db_secret="${NAME_PREFIX}-db-password"
access_secret="${NAME_PREFIX}-blob-access-key"
secret_secret="${NAME_PREFIX}-blob-secret-key"
storage_sa="${NAME_PREFIX}-storage@${PROJECT}.iam.gserviceaccount.com"

# Three outcomes, not two. "Could not ask" must never read as "no version":
# a transient auth failure answering `false` would add a SECOND version to a
# secret that already had one, and the live system would keep using the first.
has_version() {
	local out
	if ! out="$(gcloud secrets versions list "$1" --project "$PROJECT" \
		--filter="state=ENABLED" --format="value(name)" --limit=1 2>&1)"; then
		if printf '%s' "$out" | grep -qiE "NOT_FOUND|was not found"; then
			echo "secret $1 does not exist — run 'make gcp-foundation-apply' first" >&2
		else
			echo "could not list versions of $1: $out" >&2
		fi
		exit 1
	fi
	[[ -n "$out" ]]
}

# ---------------------------------------------------------------------------
# The database password.
#
# HEX, not raw bytes and not base64. The password is embedded verbatim in the
# DATABASE_URL the platform consumes, and the chart already encodes that
# constraint — templates/secret.yaml fails the render when postgresql.password
# matches [@:/?#% ]. But that guard sits inside the `if not .Values.existingSecret`
# block, so mode-2 bypasses it entirely: here, this line is the only thing
# standing between a random password and a DSN that parses into something else.
# Base64 would not do — it emits / and +.
# ---------------------------------------------------------------------------
if has_version "$db_secret"; then
	echo "ok: $db_secret already has a version (left alone)"
else
	openssl rand -hex 32 |
		gcloud secrets versions add "$db_secret" --project "$PROJECT" --data-file=- >/dev/null
	echo "created: $db_secret"
fi

# ---------------------------------------------------------------------------
# The GCS HMAC key.
#
# Both halves are written in one step, deliberately: GCS returns the secret
# exactly ONCE, at creation. If this script created the key and then failed
# before storing the secret, the key would exist, be billable, be
# indistinguishable from the real one — and be permanently unusable. So the
# create's output is captured and both secrets are written from it before
# anything else can go wrong; if either write fails, the key is deleted rather
# than left as an orphan nobody can authenticate with.
# ---------------------------------------------------------------------------
if has_version "$access_secret" && has_version "$secret_secret"; then
	echo "ok: $access_secret and $secret_secret already have versions (left alone)"
elif has_version "$access_secret" || has_version "$secret_secret"; then
	# Half a pair is worse than none: the surviving half looks like a working
	# credential. Refuse rather than guess which one is stale.
	echo "refusing: exactly one of $access_secret / $secret_secret has a version." >&2
	echo "An HMAC access id without its secret (or the reverse) cannot authenticate." >&2
	echo "Delete the GCS HMAC key and disable both secrets' versions, then re-run." >&2
	exit 1
else
	hmac_json="$(gcloud storage hmac create "$storage_sa" --project "$PROJECT" --format=json)"

	# Armed BEFORE the response is even parsed. From the instant the key exists,
	# every path out of this block that is not "both secrets stored" must delete
	# it: GCS returned the secret once, in that response, so a key we stop
	# holding the secret for is permanently unusable, billable, and
	# indistinguishable from the real one. A parse failure counts.
	access_id=""
	cleanup_key() {
		echo "rolling back: deleting the HMAC key whose secret could not be stored" >&2
		if [[ -z "$access_id" ]]; then
			# The response did not parse, so the id must be recovered by listing.
			access_id="$(gcloud storage hmac list --project "$PROJECT" \
				--filter="serviceAccountEmail=$storage_sa AND state=ACTIVE" \
				--format="value(accessId)" --limit=1 2>/dev/null || true)"
		fi
		if [[ -z "$access_id" ]]; then
			echo "COULD NOT identify the HMAC key to roll back. Delete it by hand:" >&2
			echo "  gcloud storage hmac list --project $PROJECT" >&2
			return
		fi
		gcloud storage hmac update "$access_id" --project "$PROJECT" --deactivate >/dev/null 2>&1 || true
		gcloud storage hmac delete "$access_id" --project "$PROJECT" >/dev/null 2>&1 || true
	}
	trap cleanup_key EXIT

	access_id="$(printf '%s' "$hmac_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["metadata"]["accessId"])')"

	printf '%s' "$access_id" |
		gcloud secrets versions add "$access_secret" --project "$PROJECT" --data-file=- >/dev/null
	printf '%s' "$hmac_json" |
		python3 -c 'import json,sys; sys.stdout.write(json.load(sys.stdin)["secret"])' |
		gcloud secrets versions add "$secret_secret" --project "$PROJECT" --data-file=- >/dev/null

	trap - EXIT
	echo "created: $access_secret and $secret_secret (HMAC key $access_id)"
fi

echo
echo "Next: make gcp-env-apply"
echo "It reads these ephemerally and stores none of them."
