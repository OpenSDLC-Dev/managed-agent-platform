#!/usr/bin/env bash
# Generates environment/terraform.tfvars — the other half of making a destroy or
# a rebuild portable (#478).
#
# Moving the state to a bucket is not enough on its own. Terraform evaluates the
# whole configuration before it will destroy anything, so a machine with the
# remote state but no tfvars still cannot run `terraform destroy`: it stops to
# prompt for the variables that have no default. `*.tfvars` is gitignored — the
# values name one operator's project and this repository is public (#356) — so a
# fresh checkout has none, and a generator is the only way to have both.
#
# There are exactly TWO such variables, which is why this is a script and not a
# project:
#
#   project_id                   what the operator already typed as PROJECT
#   cloud_build_service_account  looked up, because it cannot be guessed
#
# The second is the reason this is worth automating rather than documenting.
# Which account a project uses depends on when its FIRST BUILD ran, not on when
# the project was created, so there is no default that is right — variables.tf
# argues that at length. And the lookup's output needs handling: gcloud may
# print `projects/…/serviceAccounts/EMAIL` rather than the bare email, which
# Terraform's own validation rejects. Pasting it by hand is where that goes
# wrong; here it is stripped and checked before anything is written.
#
# Everything else in variables.tf has a default, so a generated file with these
# three lines is a complete, appliable configuration.
#
# Run:
#     PROJECT=your-project make gcp-env-tfvars
#
# ORDER: after `make gcp-foundation-apply`. Enabling the Cloud Build API is what
# creates the default build account, and the foundation is what enables it — on
# a project where it has never been on, the lookup fails with SERVICE_DISABLED
# rather than printing an account.

set -euo pipefail

PROJECT="${PROJECT:-}"
NAME_PREFIX="${NAME_PREFIX:-map}"
# Only written when set. environment/ and foundation/ must agree on this, and
# both default to us-central1 — so an unset value already agrees, and writing
# the default explicitly would only create a second place for it to drift. Set
# it here when the foundation was applied somewhere else. Getting it wrong is
# loud: environment/ reads the key ring through a data source, so the mismatch
# fails during PLAN, before anything is created.
KMS_LOCATION="${KMS_LOCATION:-}"
HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-$HERE/environment/terraform.tfvars}"

if [[ -z "$PROJECT" ]]; then
	cat >&2 <<-EOF
		usage: PROJECT=your-project $0

		environment:
		  PROJECT       required — the GCP project holding the environment
		  NAME_PREFIX   default "map" — must match what foundation/ was applied with
		  OUT           default environment/terraform.tfvars
	EOF
	exit 2
fi

# Refused rather than merged, and refused rather than overwritten. An existing
# file may carry variables this script knows nothing about — master_authorized_cidrs
# and the two IAP settings are the ones that matter, since all three change who
# can reach the cluster — and rewriting it with three lines would silently widen
# or close that door. There is no --force: the operator who wants a fresh one
# can delete the file, which is a decision they will have made deliberately.
if [[ -e "$OUT" ]]; then
	echo "refusing to overwrite ${OUT}" >&2
	echo "(it may carry settings this script does not generate — master_authorized_cidrs," >&2
	echo "iap_backend_service, iap_members. Delete it deliberately if you want a fresh one.)" >&2
	exit 1
fi

echo "looking up the Cloud Build default service account in ${PROJECT}"
err="$(mktemp)"
trap 'rm -f "$err"' EXIT
if ! raw="$(gcloud builds get-default-service-account --project="$PROJECT" \
	--format='value(serviceAccountEmail)' 2>"$err")"; then
	sed 's/^/  /' "$err" >&2
	case "$(cat "$err")" in
	*SERVICE_DISABLED* | *"has not been used"* | *"is disabled"*)
		echo "the Cloud Build API is not enabled in ${PROJECT}, so the account does not exist yet." >&2
		echo "Run \`PROJECT=${PROJECT} make gcp-foundation-apply\` first — enabling that API is its job." >&2
		;;
	*)
		echo "could not read the Cloud Build default service account." >&2
		;;
	esac
	exit 1
fi

# gcloud on Windows terminates its lines CRLF, and a value with the CR still
# attached is written into the file and then rejected by Terraform's validation
# with an error that shows nothing wrong. env-power.sh strips it for the same
# reason.
raw="$(printf '%s' "$raw" | tr -d '\r')"

# `projects/PROJECT/serviceAccounts/EMAIL` in some gcloud versions, the bare
# email in others. variables.tf's own validation message calls this out, which
# means it has already caught somebody.
account="${raw##*/}"

# Checked against the same shape variables.tf enforces, so this cannot write a
# file that only fails later, during an apply that has already been started.
valid=0
case "$account" in
*[[:space:]/]*) ;;
*@*.gserviceaccount.com) valid=1 ;;
esac
if [[ "$valid" -ne 1 ]]; then
	echo "the lookup returned '${raw}', which is not a service account email." >&2
	echo "Expected something like 123456789@cloudbuild.gserviceaccount.com or" >&2
	echo "123456789-compute@developer.gserviceaccount.com." >&2
	# An EMPTY value is the one worth naming, because it does not look like a
	# failure: `--format='value(KEY)'` prints nothing and exits 0 when KEY is
	# absent from the response, so a gcloud that renamed the field lands here
	# rather than erroring above, and the message otherwise points nowhere.
	if [[ -z "$raw" ]]; then
		echo "It returned nothing at all: run the command without --format to see" >&2
		echo "what it does answer — the response key may have been renamed." >&2
	fi
	exit 1
fi

cat >"$OUT" <<-EOF
	# Generated by deploy/gcp/tfvars.sh. Gitignored: these name one operator's
	# project, and this repository is public (#356).
	#
	# The two variables with no default, plus name_prefix — which does have one,
	# and is written anyway so that this file and \`make gcp-env-stop\` cannot
	# disagree about which environment they mean.
	#
	# Everything else in variables.tf has a default, so this is already a
	# complete configuration. Add your own below to override any of them.
	# master_authorized_cidrs, iap_backend_service and iap_members are the ones
	# most deployments set, and all three decide who can reach the cluster; and
	# kms_location must match what foundation/ was applied with if it is not the
	# us-central1 both default to.
	project_id                  = "${PROJECT}"
	name_prefix                 = "${NAME_PREFIX}"
	cloud_build_service_account = "${account}"
EOF

if [[ -n "$KMS_LOCATION" ]]; then
	printf 'kms_location                = "%s"\n' "$KMS_LOCATION" >>"$OUT"
fi

echo "wrote ${OUT}"
sed 's/^/  /' "$OUT"
