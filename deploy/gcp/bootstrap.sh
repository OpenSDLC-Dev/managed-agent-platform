#!/usr/bin/env bash
# Adds the first version of each Secret Manager secret the foundation created.
#
# This exists because Terraform must not (plan 20, Decision 6). A value in
# `secret_data` is a value in state, in plaintext, in both configurations —
# including the one that is destroyed and rebuilt routinely. So Terraform owns
# names, IAM bindings and preconditions, and value creation lives here.
#
# Run order:
#     make gcp-foundation-apply              # the secrets exist, empty
#     PROJECT=… make gcp-bootstrap           # this script: they get values
#     PROJECT=… make gcp-env-tfvars          # writes environment/terraform.tfvars
#     PROJECT=… make gcp-env-apply           # reads them, never storing one
#
# Since #478 `PROJECT` is half the state bucket's name, so the gcp-env-* targets
# require it; and gcp-env-apply refuses until the tfvars above exists, because
# PROJECT and that file are two independent inputs that must not disagree.
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

for tool in gcloud openssl; do
	command -v "$tool" >/dev/null || { echo "$tool is required and not on PATH" >&2; exit 2; }
done

db_secret="${NAME_PREFIX}-db-password"
db_admin_secret="${NAME_PREFIX}-db-admin-password"

# Three outcomes, not two. "Could not ask" must never read as "no version":
# a transient auth failure answering `false` would add a SECOND version to a
# secret that already had one, and the live system would keep using the first.
# So the raw probe reports all three — 0 stored, 1 definitively absent, 2 could
# not tell — and the caller decides what the third one means. has_version turns
# it into a hard stop: refusing to write beats writing a second version over a
# live one on the strength of a question that was never answered. (The split had
# a second caller until #240 — the HMAC rollback, which had to keep going and
# read "could not tell" as "destroy nothing". It is kept as two functions because
# the distinction is the point, not because two callers need it.)
#
# The question is specifically about `latest`, not about "any enabled version",
# because `latest` is what environment/'s ephemeral read resolves. A secret whose
# newest version is DISABLED but whose older one is enabled would otherwise be
# skipped here and then fail the apply — bootstrap saying "already done" about a
# version nothing will read.
#
# The state is READ, not filtered on. `--filter` is one of gcloud's list-command
# flags: `describe` rejects it at argv parsing with `unrecognized arguments`,
# before it ever reaches the API, so a `--filter` here would fail every call in
# every project — and the error text matches none of the not-found patterns
# below, so it would surface as "could not read" and abort the whole script.
probe_error=""
probe_version() {
	local out
	if ! out="$(gcloud secrets versions describe latest --secret="$1" --project "$PROJECT" \
		--format="value(state)" 2>&1)"; then
		printf '%s' "$out" | grep -qiE "NOT_FOUND|was not found|has no versions" && return 1
		probe_error="$out"
		return 2
	fi
	# Ask whether ANY line is exactly the enabled state, rather than comparing the
	# whole capture. stderr is merged in above so the error branch can classify
	# it, and gcloud writes warnings there on SUCCESS too — service-account
	# impersonation prints one on every call. Comparing the whole string would
	# read `WARNING: ...\nENABLED` as "not enabled", which reports a stored secret
	# as missing and writes a second version over a live one.
	#
	# Case is folded because gcloud renders this enum uppercase through value()
	# and lowercase in its list table.
	printf '%s\n' "$out" | grep -qix "enabled"
}

has_version() {
	local rc=0
	probe_version "$1" || rc=$?
	if [[ $rc -eq 0 ]]; then
		return 0
	fi
	if [[ $rc -eq 2 ]]; then
		echo "could not read the latest version of $1: $probe_error" >&2
		exit 1
	fi
	# Either the container is missing (the foundation was not applied) or it
	# exists and is empty, which is the normal pre-bootstrap state.
	if gcloud secrets describe "$1" --project "$PROJECT" >/dev/null 2>&1; then
		return 1
	fi
	echo "secret $1 does not exist — run 'make gcp-foundation-apply' first" >&2
	exit 1
}

# ---------------------------------------------------------------------------
# The two database passwords.
#
# HEX, not raw bytes and not base64. A password is embedded verbatim in the
# DATABASE_URL the platform consumes, and the chart already encodes that
# constraint — templates/secret.yaml fails the render when postgresql.password
# matches [@:/?#% ]. But that guard sits inside the `if not .Values.existingSecret`
# block, so mode-2 bypasses it entirely: here, this is the only thing standing
# between a random password and a DSN that parses into something else. Base64
# would not do — it emits / and +.
#
# The same generator serves both because both land in a place with the same
# constraint: the platform's password goes into a DSN, and the administrator's
# is interpolated into SQL by dbinit.sh. Hex is safe in both and needs no
# escaping in either.
# ---------------------------------------------------------------------------
ensure_password_secret() {
	local secret="$1" what="$2" password

	if has_version "$secret"; then
		echo "ok: $secret already has a version (left alone)"
		return
	fi

	# Generated and CHECKED before anything is written, rather than piped straight
	# into gcloud. In a pipeline, a failure on the left does not stop the right:
	# gcloud starts regardless, reads EOF, and can store an enabled ZERO-BYTE
	# version. `set -o pipefail` reports that pipeline as failed — after the write
	# has already landed — and every later run then skips the secret as "already
	# done", leaving an empty database password nothing will ever correct.
	#
	# The command substitution also strips the trailing newline `openssl` prints,
	# which --data-file=- would otherwise store VERBATIM as a 65th byte. That
	# never surfaces at provisioning time — google_sql_user accepts it and Cloud
	# SQL sets it — but the value is embedded in DATABASE_URL, where a raw control
	# character makes the DSN unparseable.
	#
	# `printf` is a shell builtin, so the value still never reaches a process
	# argument list where `ps` could read it.
	password="$(openssl rand -hex 32)"
	if [[ ! "$password" =~ ^[0-9a-f]{64}$ ]]; then
		echo "openssl did not produce 64 hex characters — refusing to store it" >&2
		exit 1
	fi
	printf '%s' "$password" |
		gcloud secrets versions add "$secret" --project "$PROJECT" --data-file=- >/dev/null
	echo "created: $secret ($what)"
}

ensure_password_secret "$db_secret" "the platform's own database role"
ensure_password_secret "$db_admin_secret" "the Cloud SQL built-in administrator"

echo
echo "Next: PROJECT=$PROJECT make gcp-env-tfvars    # writes environment/terraform.tfvars"
echo "Then: PROJECT=$PROJECT make gcp-env-apply     # reads these ephemerally, stores none"
