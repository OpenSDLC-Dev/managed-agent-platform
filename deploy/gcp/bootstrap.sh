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
# So the raw probe reports all three — 0 stored, 1 definitively absent, 2 could
# not tell — and each caller decides what the third one means. has_version turns
# it into a hard stop; the rollback below cannot afford to stop, and reads it as
# "destroy nothing".
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
	db_password="$(openssl rand -hex 32)"
	if [[ ! "$db_password" =~ ^[0-9a-f]{64}$ ]]; then
		echo "openssl did not produce 64 hex characters — refusing to store it" >&2
		exit 1
	fi
	printf '%s' "$db_password" |
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
# anything else can go wrong; if the pair did not reach Secret Manager, the key
# is deleted rather than left as an orphan nobody can authenticate with.
#
# "Did not reach" is a question for the SERVER, not for the exit status. A write
# can commit and still report failure, and rolling that back would destroy the
# working key while leaving both secrets in place — so the rollback re-reads them
# and deletes nothing it cannot prove is unusable.
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
	# Snapshot first. The rollback below may have to identify the new key by
	# listing, and "the one active key" is not a safe answer: this service
	# account may already own an active HMAC key created out of band, and
	# deleting THAT would destroy a working credential — the precise failure this
	# whole script exists to avoid. Only a key that was not here a moment ago is
	# ours to delete.
	before="$(gcloud storage hmac list --project "$PROJECT" \
		--filter="serviceAccountEmail=$storage_sa" --format="value(accessId)" | sort)"

	# Armed BEFORE the create, not after it. `gcloud storage hmac create` can
	# commit server-side and still report failure, and a key born in that window
	# is precisely the orphan this trap exists for — billable, valid-looking, and
	# permanently unusable, because its secret was in the response that was lost.
	# Arming afterwards would leave that one window uncovered, with no banner and
	# no recovery instructions. The snapshot above is what lets the handler find
	# such a key without an access id to go on.
	#
	# From the instant the key can exist, every path out of this block that is not
	# "both secrets stored" must delete it. A parse failure counts.
	access_id=""
	cleanup_key() {
		# The trap inherits `set -euo pipefail`, and every probe below is allowed
		# to fail. Without this line a failing rollback probe would abort the trap
		# half-way through — after the banner had announced a deletion, and before
		# the manual-recovery instructions were printed.
		set +e

		# Ask the server what is actually stored before destroying anything.
		#
		# The dangerous case is a write that COMMITTED and then reported failure:
		# a dropped connection while reading the response, or a Ctrl-C landing
		# just after the last write. The pair is then stored and correct, and
		# deleting the key would strand two secrets that look entirely valid —
		# after which the next run's `already have versions (left alone)` skip
		# would bless a credential that authenticates against nothing, and the
		# platform would fail at runtime rather than here. Only a pair that is
		# demonstrably NOT both stored is safe to roll back.
		local a=0 s=0
		probe_version "$access_secret" || a=$?
		probe_version "$secret_secret" || s=$?
		if [[ $a -eq 0 && $s -eq 0 ]]; then
			echo "NOT rolling back: both secrets hold a version, so the HMAC pair is stored" >&2
			echo "and the key is the working one. Re-run to confirm — it will skip." >&2
			return
		fi
		if [[ $a -eq 2 || $s -eq 2 ]]; then
			echo "COULD NOT read the secrets back, and will NOT delete an HMAC key that may be" >&2
			echo "the working one. Check all three by hand before re-running:" >&2
			echo "  gcloud secrets versions describe latest --secret=$access_secret --project $PROJECT" >&2
			echo "  gcloud secrets versions describe latest --secret=$secret_secret --project $PROJECT" >&2
			echo "  gcloud storage hmac list --project $PROJECT --filter=serviceAccountEmail=$storage_sa" >&2
			return
		fi

		if [[ -z "$access_id" ]]; then
			# No id to go on — the create failed, or committed and reported
			# failure, or its response did not parse. Recover the key as the
			# difference against the snapshot, and act only on an unambiguous one.
			local after new listed=1
			after="$(gcloud storage hmac list --project "$PROJECT" \
				--filter="serviceAccountEmail=$storage_sa" --format="value(accessId)" 2>/dev/null | sort)" || listed=0
			if [[ $listed -eq 1 ]]; then
				new="$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after") | grep -c .)"
				if [[ "$new" == "0" ]]; then
					# Nothing was added, so there is nothing to roll back — the
					# ordinary case when the create itself failed. Saying anything
					# here would be crying wolf.
					return
				fi
				if [[ "$new" == "1" ]]; then
					access_id="$(comm -13 <(printf '%s\n' "$before") <(printf '%s\n' "$after"))"
				fi
			fi
			# A failed list, or more than one new key, falls through to the
			# refuse-and-instruct block below. Both are states where guessing
			# could delete a working credential.
		fi
		if [[ -z "$access_id" ]]; then
			echo "COULD NOT identify the key to roll back, and will NOT guess — deleting the" >&2
			echo "wrong HMAC key would destroy a working credential. Delete it by hand:" >&2
			echo "  gcloud storage hmac list --project $PROJECT --filter=serviceAccountEmail=$storage_sa" >&2
			return
		fi
		# Announced only now that the outcome is known, so the log can never
		# assert a deletion that did not happen.
		echo "rolling back: deleting HMAC key $access_id, whose secret could not be stored" >&2
		# A key must be INACTIVE before it can be deleted, so a failed deactivate
		# guarantees a failed delete. Discarding both statuses would print
		# "rolling back" over a key that is still there, still billable, and still
		# unusable — with the id never shown, so nothing tells the operator what
		# to clean up or that there is anything to clean up at all.
		if ! gcloud storage hmac update "$access_id" --project "$PROJECT" --deactivate >/dev/null 2>&1 ||
			! gcloud storage hmac delete "$access_id" --project "$PROJECT" >/dev/null 2>&1; then
			echo "ROLLBACK FAILED: HMAC key $access_id still exists and cannot authenticate." >&2
			echo "It is billable. Delete it by hand (deactivate first — an active key cannot be deleted):" >&2
			echo "  gcloud storage hmac update $access_id --project $PROJECT --deactivate" >&2
			echo "  gcloud storage hmac delete $access_id --project $PROJECT" >&2
		fi

		# One half can commit before the other fails. The key is gone, so that
		# leftover version is now a credential to nothing — and the next run would
		# stop at the generic "exactly one of ..." refusal without saying which
		# secret is at fault. Name it, and give the command that clears it.
		if [[ $a -eq 0 || $s -eq 0 ]]; then
			echo "NOTE: a version was written before the failure and will block the next run." >&2
			echo "It cannot authenticate — its key no longer exists. Disable it with:" >&2
			[[ $a -eq 0 ]] && echo "  gcloud secrets versions disable latest --secret=$access_secret --project $PROJECT" >&2
			[[ $s -eq 0 ]] && echo "  gcloud secrets versions disable latest --secret=$secret_secret --project $PROJECT" >&2
		fi
	}
	trap cleanup_key EXIT

	hmac_json="$(gcloud storage hmac create "$storage_sa" --project "$PROJECT" --format=json)"

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
