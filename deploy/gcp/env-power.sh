#!/usr/bin/env bash
# Parks the staging environment — both node pools to zero, Cloud SQL to
# activation policy NEVER — and revives it. What it parks, what keeps billing
# regardless, and why parking a cluster is safe here at all are in this
# directory's README under "Parking it between uses". What follows is what
# someone editing this script has to know.
#
# WHY NOT `terraform destroy`: it takes Cloud SQL with it, and vault ciphertext
# lives only in Postgres. This changes nothing about which resources exist.
#
# WHY NO TERRAFORM, STATE OR TFVARS: parking is the operation you want from
# whatever machine you are at, so every coordinate is derived from PROJECT and
# NAME_PREFIX and the rest is discovered. It is gcloud and nothing else.
#
# But note the converse, because it is a trap: `node_count` IS declared in
# environment/main.tf and Cloud SQL's `activation_policy` is NOT. So a
# `terraform apply` while parked scales the pools back up and leaves the
# database stopped — the one ordering this script exists to prevent. Revive with
# `start` before applying.
#
# WHERE THE SAVED SIZES LIVE. `stop` records each pool's node count in a
# `power-saved-<pool>` CLUSTER RESOURCE LABEL, and `start` reads them back. The
# sizes travel with the cluster rather than with a laptop, so whoever revives it
# need not be whoever parked it. If they are ever missing, `start` REFUSES and
# asks for an explicit NODES rather than guessing: variables.tf defaults to two
# nodes a pool, and a guess in that direction doubles the bill the parking was
# for while looking perfectly healthy.
#
# `--update-labels` REPLACES the cluster's whole label set. It is not a merge,
# whatever the name suggests: gcloud builds the SetLabelsRequest from the flag's
# pairs alone and reads the cluster only for its fingerprint
# (googlecloudsdk/api_lib/container/api_adapter.py, UpdateLabelsCommon). So
# every write below sends the complete set the cluster should end up with —
# ours and everyone else's. Sending only ours would delete
# goog-terraform-provisioned, and on a resumed stop would delete the size the
# interrupted attempt had already saved. `--remove-labels` is the read-modify-
# write half and is safe for deletion, but it errors on a label that is not
# there, so it is only ever handed keys read back in the same run.
#
# ORDER, both ways, and it is why these are not four independent knobs: going
# down the nodes drain before the database goes; coming up the database is
# RUNNABLE before any node returns. `make gcp-power-test` asserts both.
#
# Run:
#     PROJECT=your-project make gcp-env-stop
#     PROJECT=your-project make gcp-env-start
#     PROJECT=your-project make gcp-env-status

set -euo pipefail

ACTION="${1:-}"
PROJECT="${PROJECT:-}"
NAME_PREFIX="${NAME_PREFIX:-map}"
# Only consulted when a pool has no saved size. Not a default: unset is what
# makes `start` refuse rather than guess.
NODES="${NODES:-}"
# How long to wait for Cloud SQL after starting it, in seconds. A cold start is
# minutes, not seconds.
SQL_TIMEOUT="${SQL_TIMEOUT:-600}"
# The gap between polls. Exists so `env_power_test.py` can exercise the wait —
# both reaching RUNNABLE and giving up — in seconds rather than in ten minutes.
SQL_POLL="${SQL_POLL:-10}"

CLUSTER="${NAME_PREFIX}-staging"
SQL_INSTANCE="${NAME_PREFIX}-staging"
LABEL_PREFIX="power-saved-"

usage() {
	cat >&2 <<-EOF
		usage: PROJECT=your-project $0 {stop|start|status}

		  stop     resize every node pool to zero, then stop Cloud SQL
		  start    start Cloud SQL, wait for it, then restore the node pools
		  status   report what is parked and what is running

		environment:
		  PROJECT       required — the GCP project holding the environment
		  NAME_PREFIX   default "map" — resources are \${NAME_PREFIX}-staging
		  NODES         only for start, and only when a saved size is gone
		  SQL_TIMEOUT   default 600 — seconds to wait for Cloud SQL on start
	EOF
	exit 2
}

case "$ACTION" in
stop | start | status) ;;
*) usage ;;
esac

if [[ -z "$PROJECT" ]]; then
	echo "PROJECT is required: PROJECT=my-project make gcp-env-${ACTION}" >&2
	exit 2
fi

# `tr -d '\r'` because gcloud on Windows terminates its lines CRLF, and a pool
# name read with the CR still attached produces `node pool "platform\r" not
# found` on every call that follows. Observed, not theorised. The pipe does not
# hide a failure: `pipefail` is set, so gcloud's status is still the pipeline's.
g() { gcloud --project="$PROJECT" "$@" | tr -d '\r'; }

# Discovered rather than configured: variables.tf gives `zone` no default, so
# there is no value to inherit, and `--location` covers zonal and regional alike.
#
# stderr is deliberately NOT suppressed and the failure is deliberately NOT
# swallowed. An expired token, a missing permission and a wrong project are not
# "no such cluster", and printing the prefix hint for them sends the operator to
# fix something that was never wrong. Under `set -e` a real failure aborts here
# carrying gcloud's own message; a filter that simply matches nothing exits 0
# with empty output, and that — and only that — is the case below.
LOCATION="$(g container clusters list --filter="name=${CLUSTER}" \
	--format="value(location)")"
if [[ -z "$LOCATION" ]]; then
	echo "no cluster named ${CLUSTER} in project ${PROJECT}" >&2
	echo "(NAME_PREFIX=${NAME_PREFIX}; set it if this deployment uses another prefix)" >&2
	exit 1
fi

# Pools are discovered too, so one added to main.tf later is parked without this
# script being edited. Read into a variable first rather than straight into a
# `while` over a process substitution: that form cannot see gcloud's exit status,
# so a listing that printed one pool and then failed would leave the other one
# running while `stop` went on to shut the database down. The assignment is what
# makes `set -e` catch it.
POOLS_RAW="$(g container node-pools list --cluster="$CLUSTER" \
	--location="$LOCATION" --format="value(name)")"
POOLS=()
while IFS= read -r pool; do
	if [[ -n "$pool" ]]; then POOLS+=("$pool"); fi
done <<< "$POOLS_RAW"
if [[ ${#POOLS[@]} -eq 0 ]]; then
	echo "cluster ${CLUSTER} reports no node pools — refusing to act" >&2
	exit 1
fi

pool_size() {
	g container node-pools describe "$1" --cluster="$CLUSTER" \
		--location="$LOCATION" --format="value(initialNodeCount)"
}

sql_state() {
	g sql instances describe "$SQL_INSTANCE" --format="value(state)"
}

saved_size() {
	g container clusters describe "$CLUSTER" --location="$LOCATION" \
		--format="value(resourceLabels.${LABEL_PREFIX}${1})"
}

cluster_labels() {
	g container clusters describe "$CLUSTER" --location="$LOCATION" \
		--format="value(resourceLabels)"
}

# gcloud renders a dict under `value()` as `k=v;k=v`. Splitting on ';' is
# unambiguous because a GCP label may hold only lowercase letters, digits, '-'
# and '_' — neither a key nor a value can contain the separator. Prints one
# `k=v` per line.
label_lines() {
	local rest="$1" kv
	while [[ -n "$rest" ]]; do
		kv="${rest%%;*}"
		if [[ "$kv" == "$rest" ]]; then rest=""; else rest="${rest#*;}"; fi
		if [[ -n "$kv" ]]; then printf '%s\n' "$kv"; fi
	done
}

case "$ACTION" in

status)
	# Every value is assigned before it is printed. Inside a `printf` argument a
	# failed call is invisible to `set -e` — the builtin succeeds — so status
	# would report blank counts and exit 0 on an environment it never reached.
	echo "project      ${PROJECT}"
	echo "cluster      ${CLUSTER} (${LOCATION})"
	for p in "${POOLS[@]}"; do
		size="$(pool_size "$p")"
		saved="$(saved_size "$p")"
		printf 'pool %-10s %s node(s)%s\n' "$p" "$size" \
			"${saved:+, parked from ${saved}}"
	done
	state="$(sql_state)"
	echo "cloud sql    ${SQL_INSTANCE} ${state}"
	;;

stop)
	running=0
	for p in "${POOLS[@]}"; do
		size="$(pool_size "$p")"
		if [[ "$size" != "0" ]]; then running=1; fi
	done
	state="$(sql_state)"

	if [[ "$running" == "0" && "$state" == "STOPPED" ]]; then
		echo "already parked — nothing to do"
		existing="$(cluster_labels)"
		case ";${existing}" in
		*";${LABEL_PREFIX}"*) ;;
		*)
			echo
			echo "warning: no saved sizes are recorded on this cluster, so it was not" >&2
			echo "parked by this script — \`start\` will need NODES=<n> to revive it." >&2
			;;
		esac
		exit 0
	fi

	existing="$(cluster_labels)"

	# The complete set the cluster should end up with, because the write replaces
	# rather than merges. Everyone else's labels first, verbatim.
	desired=""
	while IFS= read -r kv; do
		if [[ -z "$kv" ]]; then continue; fi
		case "${kv%%=*}" in
		"${LABEL_PREFIX}"*) continue ;;
		esac
		desired="${desired:+${desired},}${kv}"
	done <<< "$(label_lines "$existing")"

	# Then ours. An already-saved size wins over the pool's current count, which
	# is what makes a resumed stop keep the first attempt's number instead of
	# recording the zero it left behind.
	for p in "${POOLS[@]}"; do
		keep="$(saved_size "$p")"
		if [[ -z "$keep" ]]; then
			size="$(pool_size "$p")"
			if [[ "$size" != "0" ]]; then keep="$size"; fi
		fi
		if [[ -n "$keep" ]]; then
			desired="${desired:+${desired},}${LABEL_PREFIX}${p}=${keep}"
		fi
	done

	echo "saving pool sizes on the cluster: ${desired}"
	g container clusters update "$CLUSTER" --location="$LOCATION" \
		--update-labels="$desired" --quiet

	for p in "${POOLS[@]}"; do
		size="$(pool_size "$p")"
		if [[ "$size" == "0" ]]; then
			echo "pool ${p} already at zero"
			continue
		fi
		echo "resizing pool ${p} to zero (nodes are drained first)"
		g container clusters resize "$CLUSTER" --location="$LOCATION" \
			--node-pool="$p" --num-nodes=0 --quiet
	done

	state="$(sql_state)"
	if [[ "$state" == "STOPPED" ]]; then
		echo "cloud sql ${SQL_INSTANCE} already stopped"
	else
		echo "stopping cloud sql ${SQL_INSTANCE}"
		g sql instances patch "$SQL_INSTANCE" --activation-policy=NEVER --quiet
	fi

	echo
	echo "parked. the nodes, their disks and the database instance have stopped"
	echo "billing; storage, the control plane and the LoadBalancers have not."
	;;

start)
	# Resolved for EVERY parked pool before anything is touched, and Cloud SQL is
	# not started until it is. Refusing halfway would leave the environment part
	# live and billing while reporting that it refused.
	existing="$(cluster_labels)"
	targets=""
	missing=""
	for p in "${POOLS[@]}"; do
		current="$(pool_size "$p")"
		if [[ "$current" != "0" ]]; then continue; fi
		want="$(saved_size "$p")"
		if [[ -z "$want" ]]; then want="$NODES"; fi
		if [[ -z "$want" ]]; then
			missing="${missing:+${missing}, }${p}"
			continue
		fi
		case "$want" in
		'' | *[!0-9]*)
			echo "size for pool ${p} is not a number: '${want}'" >&2
			exit 2
			;;
		esac
		# Zero is rejected rather than obeyed. `NODES=0` would resize to nothing,
		# then clear the labels and announce "running" — leaving the environment
		# parked with its parked marker gone, which is the one state neither
		# `start` nor CD can recover from without an operator.
		if [[ "$want" -lt 1 ]]; then
			echo "size for pool ${p} must be at least 1, got ${want}" >&2
			exit 2
		fi
		targets="${targets:+${targets} }${p}:${want}"
	done

	if [[ -n "$missing" ]]; then
		echo "no saved size for: ${missing} — and NODES is unset, so refusing to guess" >&2
		echo "(variables.tf defaults to two nodes a pool; that may not be this one's size)" >&2
		echo "re-run as: NODES=<n> PROJECT=${PROJECT} make gcp-env-start" >&2
		exit 1
	fi

	state="$(sql_state)"
	if [[ "$state" == "STOPPED" ]]; then
		echo "starting cloud sql ${SQL_INSTANCE}"
		g sql instances patch "$SQL_INSTANCE" --activation-policy=ALWAYS --quiet
	else
		echo "cloud sql ${SQL_INSTANCE} already running"
	fi

	# Waited for rather than assumed: the patch returns when the operation is
	# accepted, and the instance reaches RUNNABLE later. RUNNABLE is the state the
	# API reports, not a proof that Postgres is accepting connections — it is the
	# closest signal available here, and it is far better than not waiting.
	waited=0
	state="$(sql_state)"
	while [[ "$state" != "RUNNABLE" ]]; do
		if [[ "$waited" -ge "$SQL_TIMEOUT" ]]; then
			echo "cloud sql is still ${state} after ${SQL_TIMEOUT}s — pools left at zero" >&2
			echo "(the instance was asked to start and still is; re-run start once it" >&2
			echo "reaches RUNNABLE. No pool was resized, and the saved sizes are intact.)" >&2
			exit 1
		fi
		sleep "$SQL_POLL"
		waited=$((waited + SQL_POLL))
		state="$(sql_state)"
	done
	echo "cloud sql is RUNNABLE after ${waited}s"

	for t in $targets; do
		p="${t%%:*}"
		size="${t#*:}"
		echo "restoring pool ${p} to ${size}"
		g container clusters resize "$CLUSTER" --location="$LOCATION" \
			--node-pool="$p" --num-nodes="$size" --quiet
	done

	# Cleared from a FRESH read of what the cluster still carries, not from the
	# list of pools this run happened to resize. A start that was interrupted
	# after some pools came back would otherwise leave those pools' labels behind
	# for good: the re-run finds them already running, skips them, and clears
	# nothing — and a leftover label tells CD the environment is parked, so every
	# deploy afterwards is skipped with a notice while staging is fully up.
	remaining="$(cluster_labels)"
	drop=""
	while IFS= read -r kv; do
		if [[ -z "$kv" ]]; then continue; fi
		case "${kv%%=*}" in
		"${LABEL_PREFIX}"*) drop="${drop:+${drop},}${kv%%=*}" ;;
		esac
	done <<< "$(label_lines "$remaining")"
	if [[ -n "$drop" ]]; then
		g container clusters update "$CLUSTER" --location="$LOCATION" \
			--remove-labels="$drop" --quiet
	fi

	echo
	echo "running. pods reschedule themselves — nothing needs redeploying."
	echo "staging is at whatever commit it was parked on; to catch it up to main:"
	echo "    gh workflow run deploy.yml --ref main"
	;;
esac
