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
# It has a second mode, `--check`, which reads the file back instead of writing
# it and asserts it still describes the PROJECT and NAME_PREFIX the caller named.
# That is not a tidiness check: those two choose the state BUCKET while the file
# chooses the RESOURCES, so the targets that touch the backend run it first. The
# long argument is at the mode itself.
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
		usage: PROJECT=your-project $0 [--check]

		  (no argument)  generate OUT
		  --check        assert an existing OUT still describes PROJECT/NAME_PREFIX

		environment:
		  PROJECT       required — the GCP project holding the environment
		  NAME_PREFIX   default "map" — must match what foundation/ was applied with
		  OUT           default environment/terraform.tfvars
	EOF
	exit 2
fi

# --check answers the question that the split between "where the state lives" and
# "what the configuration builds" opens up, and that nothing else asks.
#
# The state bucket is chosen from PROJECT and NAME_PREFIX at `terraform init`.
# The RESOURCES come from project_id and name_prefix in this file. Those are two
# independent inputs to one operation, and a mismatch is not a failure — it is a
# success against the wrong state. `PROJECT=b make gcp-env-apply` in a checkout
# whose tfvars says project a loads b's state and configures a's resources; if
# both exist, the plan destroys what b's state records and creates it in a. The
# custom-prefix case is worse because the DOCS walk into it: generate with
# NAME_PREFIX=acme, then run the documented `PROJECT=... make gcp-env-apply`, and
# the bucket is `...-map-tfstate` while every resource is an `acme` one.
#
# So the targets that touch the backend assert the two agree first. Nothing here
# talks to GCP: it is a comparison between a file and two variables.

# An unrecognised argument is refused rather than ignored. Falling through to
# generate mode meant `--chek` silently WROTE a file on a fresh checkout — the
# opposite of what the operator asked for, from a single transposed letter.
if [[ $# -gt 1 || ( $# -eq 1 && "${1:-}" != "--check" ) ]]; then
	echo "unknown argument: $*" >&2
	echo "usage: PROJECT=… $0 [--check]" >&2
	exit 2
fi

if [[ "${1:-}" == "--check" ]]; then
	if [[ ! -e "$OUT" ]]; then
		echo "no ${OUT}" >&2
		echo "Terraform evaluates the whole configuration before it will apply OR destroy," >&2
		echo "and these variables have no default, so it would stop to prompt." >&2
		echo "Run \`PROJECT=${PROJECT} make gcp-env-tfvars\` first." >&2
		exit 1
	fi

	# `*.auto.tfvars` is a SECOND source of the same variables, loaded
	# automatically and after this file, so it can set project_id or name_prefix
	# where this check cannot see it. Nothing in this repository writes one, so
	# its presence means somebody layered inputs deliberately — and a guard that
	# silently stops covering the thing it names is worse than no guard. Refuse
	# and say so. (`-var` and TF_CLI_ARGS_* can do the same and arrive after this
	# process has exited; those are out of reach of any check here, and this
	# comment is the honest limit of what --check promises.)
	shopt -s nullglob
	autos=("$(dirname "$OUT")"/*.auto.tfvars "$(dirname "$OUT")"/*.auto.tfvars.json)
	shopt -u nullglob
	if [[ ${#autos[@]} -gt 0 ]]; then
		echo "refusing: ${autos[*]} would be loaded too, after ${OUT}." >&2
		echo "This check reads only ${OUT}, so it cannot tell you which project_id and" >&2
		echo "name_prefix Terraform will actually use. Fold them into ${OUT} or remove them." >&2
		exit 1
	fi

	# One assignment per variable: Terraform rejects a file that assigns the same
	# one twice, so there is nothing to disambiguate here.
	#
	# Comments are stripped FIRST, and `/* */` is the reason. An assignment inside
	# a block comment is invisible to Terraform and — before this — visible to a
	# line-oriented sed, so `project_id = "prod"` beside a commented-out
	# `name_prefix = "acme"` read as agreeing with NAME_PREFIX=acme while Terraform
	# used the default. That pairs one environment's bucket with another's
	# resources with the guard green, which is the whole failure it exists to stop.
	strip_comments() {
		awk '
			{ line = $0; out = ""; i = 1
			  while (i <= length(line)) {
			    c = substr(line, i, 1); d = substr(line, i, 2)
			    if (inblock) { if (d == "*/") { inblock = 0; i += 2 } else i++ ; continue }
			    if (inquote) { out = out c; if (c == "\\") { out = out substr(line, i+1, 1); i += 2; continue }
			                   if (c == "\"") inquote = 0; i++; continue }
			    if (d == "/*") { inblock = 1; i += 2; continue }
			    if (d == "//" || c == "#") break
			    if (c == "\"") inquote = 1
			    out = out c; i++
			  }
			  print out }
		' "$OUT"
	}
	tfvar() {
		strip_comments |
			sed -n "s/^[[:space:]]*$1[[:space:]]*=[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
	}
	got_project="$(tfvar project_id)"
	# Absent means the variables.tf default, which is the same "map" this script
	# defaults NAME_PREFIX to — so a hand-written tfvars that omits it agrees.
	# An explicit `name_prefix = ""` reads the same way here, which is imprecise
	# and harmless: variables.tf requires `^[a-z][a-z0-9-]{0,16}$`, so Terraform
	# rejects the empty string at plan. A loud failure, not a wrong-state one.
	got_prefix="$(tfvar name_prefix)"
	got_prefix="${got_prefix:-map}"

	bad=0
	if [[ "$got_project" != "$PROJECT" ]]; then
		echo "${OUT} says project_id = \"${got_project}\", but you passed PROJECT=${PROJECT}." >&2
		bad=1
	fi
	if [[ "$got_prefix" != "$NAME_PREFIX" ]]; then
		echo "${OUT} says name_prefix = \"${got_prefix}\", but you passed NAME_PREFIX=${NAME_PREFIX}." >&2
		bad=1
	fi
	if [[ "$bad" -ne 0 ]]; then
		echo >&2
		echo "Those two choose the state bucket; the file chooses the resources. Applying or" >&2
		echo "destroying with them disagreeing operates on one environment's state while" >&2
		echo "configuring another's. Pass the values the file names, or regenerate it." >&2
		exit 1
	fi
	echo "${OUT} agrees with PROJECT=${PROJECT} NAME_PREFIX=${NAME_PREFIX}"
	exit 0
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
# Set once the destination temporary exists; see the atomic write at the bottom.
tmp=""
cleanup() {
	rm -f "$err"
	[[ -n "$tmp" ]] && rm -f "$tmp"
	return 0
}
trap cleanup EXIT
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

# The SAME shape variables.tf enforces, character for character — not a looser
# approximation of it. `*@*.gserviceaccount.com` was the approximation, and it
# accepted three things Terraform rejects: `a@b@c.gserviceaccount.com` (a second
# `@`), `@x.gserviceaccount.com` and `x@.gserviceaccount.com` (an empty half).
# Writing a value Terraform will reject is the one outcome this script exists to
# prevent, so the check has to be the same check. tfvars_test.py asserts the two
# agree over a corpus rather than trusting this comment: it reads the regex out
# of variables.tf and requires that what this script accepts is exactly what
# that regex accepts.
#
# It is deliberately a STRICT SUBSET of that regex rather than a copy of it, and
# the difference is the characters Terraform's pattern happens to allow but a
# quoted HCL string cannot survive. `[^@ /]` admits `"`, `\`, `$` and `%`, so
# `x"@y.gserviceaccount.com` passes validation and is then emitted verbatim
# between quotes — a syntax error, or worse a `${...}`/`%{...}` template that
# evaluates. No real service account contains any of them: the three shapes GCP
# issues are PROJECT_NUMBER@cloudbuild, PROJECT_NUMBER-compute@developer, and
# NAME@PROJECT.iam. Rejecting output that cannot be one of those is the promise
# this script makes; matching Terraform exactly would be keeping a bug for
# symmetry. tfvars_test.py asserts the subset direction — that nothing this
# accepts is rejected by variables.tf — over a corpus, in that direction only.
#
# Kept in a variable because `[[ =~ ]]` matches a QUOTED right-hand side as a
# literal string; an unquoted expansion is the idiom, and `[[` does not word-split.
ACCOUNT_RE='^[A-Za-z0-9._+-]+@[A-Za-z0-9.-]+\.(iam\.)?gserviceaccount\.com$'
if [[ ! "$account" =~ $ACCOUNT_RE ]]; then
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

# Written beside the destination and moved into place only once it is COMPLETE,
# because a partial file here is worse than none: this script refuses to
# overwrite an existing file, so an interrupt between the heredoc and the
# kms_location append below would leave a plausible-looking tfvars carrying the
# wrong KMS location, and every retry would refuse to correct it. `mv` within one
# directory is atomic, which is why the temporary cannot go in /tmp.
tmp="$(mktemp "${OUT}.XXXXXX")"
cat >"$tmp" <<-EOF
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
	printf 'kms_location                = "%s"\n' "$KMS_LOCATION" >>"$tmp"
fi

# The file lands 0600 rather than the umask default, because that is what mktemp
# makes. Left alone: only the operator running Terraform reads it, and stricter
# is the safe direction for a file naming a project and a service account.
mv "$tmp" "$OUT"
tmp=""

echo "wrote ${OUT}"
sed 's/^/  /' "$OUT"
