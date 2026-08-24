#!/usr/bin/env bash
# Powers the staging environment down to near-zero cost, and back up again.
#
# WHY THIS IS NOT `terraform destroy`. A destroy takes Cloud SQL with it, and
# the vault ciphertext lives only in Postgres — see "What a destroy actually
# costs" in this directory's README. This script changes nothing about WHICH
# resources exist; it only parks the two that bill by the hour. Terraform's
# state stays accurate across a power cycle, because the inventory it describes
# is unchanged. That is also why this script needs no state, no tfvars and no
# Terraform binary: it is gcloud and nothing else, so it runs anywhere the
# operator's credentials do.
#
# WHAT IT PARKS, AND WHAT IT CANNOT.
#
#   parked   the two node pools, resized to zero — with them go the nodes, their
#            boot disks, and Cloud NAT's per-VM charge
#   parked   Cloud SQL, activation policy NEVER — "stopping an instance suspends
#            instance charges", storage and the IP address keep billing
#   stays    the GKE control plane, $0.10/cluster-hour, which one zonal cluster's
#            free-tier credit covers
#   stays    the LoadBalancer forwarding rules, including the console's two
#
# THE CLUSTER IS THE SAFE THING TO PARK because mode 2 puts every piece of state
# outside it: Postgres is Cloud SQL, blobs are GCS, the credential cipher is
# Cloud KMS. `kubectl get pvc -A` is empty by design. So a node is genuinely
# disposable and `start` needs no restore step — the control plane, etcd,
# Deployments and Helm release records never went away, so pods reschedule
# themselves the moment nodes come back.
#
# The console shares this cluster and is parked and revived along with the
# platform, without being named here. Its two external LoadBalancers are not
# parked and keep billing; taking those down would mean deleting a Service,
# which is a workload change and not a power operation.
#
# WHERE THE SAVED SIZES LIVE. `stop` writes each pool's node count into a
# CLUSTER RESOURCE LABEL before zeroing it, and `start` reads them back. The
# saved sizes therefore travel with the cluster rather than with a machine,
# which is the whole point — a colleague on another laptop can revive what you
# parked. Terraform does not declare `resource_labels` on this cluster, so the
# labels survive its applies; if they are ever lost anyway, `start` REFUSES and
# asks for an explicit size rather than guessing one. Guessing is the one
# failure mode worth designing against: `variables.tf` defaults to two nodes a
# pool and this environment runs one, so a guess would silently double the bill
# the script exists to cut.
#
# Order matters in both directions and is the reason these are not two
# independent knobs. Going down, nodes first: a pod that loses its database
# mid-query is a worse way to stop than one that is drained. Coming up, the
# database first: pods that land before Postgres answers spend their first
# minutes in CrashLoopBackOff.
#
# Run:
#     PROJECT=your-project make gcp-env-stop
#     PROJECT=your-project make gcp-env-start
#     PROJECT=your-project make gcp-env-status

set -euo pipefail

ACTION="${1:-}"
PROJECT="${PROJECT:-}"
NAME_PREFIX="${NAME_PREFIX:-map}"
# Only consulted when the saved labels are missing. Not a default size: unset is
# what makes `start` refuse rather than guess.
NODES="${NODES:-}"
# How long to wait for Cloud SQL to answer after starting it, in seconds. A cold
# start is minutes, not seconds, and the wait is what keeps `start` ordered.
SQL_TIMEOUT="${SQL_TIMEOUT:-600}"
# The gap between polls. Exists so `env_power_test.py` can exercise the wait —
# both reaching RUNNABLE and giving up — in seconds rather than in the ten
# minutes an operator would actually sit through.
SQL_POLL="${SQL_POLL:-10}"

CLUSTER="${NAME_PREFIX}-staging"
SQL_INSTANCE="${NAME_PREFIX}-staging"
LABEL_PREFIX="power-saved-"

usage() {
	cat >&2 <<-EOF
		usage: PROJECT=your-project $0 {stop|start|status}

		  stop     resize both node pools to zero, then stop Cloud SQL
		  start    start Cloud SQL, wait for it, then restore the node pools
		  status   report what is parked and what is running

		environment:
		  PROJECT       required — the GCP project holding the environment
		  NAME_PREFIX   default "map" — resources are \${NAME_PREFIX}-staging
		  NODES         only for start, and only when the saved sizes are gone
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

g() { gcloud --project="$PROJECT" "$@"; }

# The cluster's location is discovered rather than configured. `variables.tf`
# gives `zone` no default, so there is no value to inherit, and asking the
# operator for one they would have to look up defeats the point of a script that
# runs anywhere. `--location` covers zonal and regional alike.
LOCATION="$(g container clusters list --filter="name=${CLUSTER}" \
	--format="value(location)" 2>/dev/null || true)"
if [[ -z "$LOCATION" ]]; then
	echo "no cluster named ${CLUSTER} in project ${PROJECT}" >&2
	echo "(NAME_PREFIX=${NAME_PREFIX}; set it if this deployment uses another prefix)" >&2
	exit 1
fi

# Discovered, not hard-coded to platform/sandbox: a pool added to main.tf later
# is then parked by this script without it being edited, and a pool removed does
# not leave it resizing something that is gone.
#
# Read with a loop rather than `mapfile`, which needs bash 4 — see the note on
# `make gcp-lint`, which bans it because macOS still ships 3.2 and these scripts
# are run from a laptop. A failed list leaves POOLS empty rather than aborting,
# which the guard below turns into a refusal instead of a no-op.
POOLS=()
while IFS= read -r pool; do
	POOLS+=("$pool")
done < <(g container node-pools list --cluster="$CLUSTER" \
	--location="$LOCATION" --format="value(name)")
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

case "$ACTION" in

status)
	echo "project      ${PROJECT}"
	echo "cluster      ${CLUSTER} (${LOCATION})"
	for p in "${POOLS[@]}"; do
		saved="$(saved_size "$p")"
		printf 'pool %-10s %s node(s)%s\n' "$p" "$(pool_size "$p")" \
			"${saved:+, parked from ${saved}}"
	done
	echo "cloud sql    ${SQL_INSTANCE} $(sql_state)"
	;;

stop)
	running=0
	for p in "${POOLS[@]}"; do
		if [[ "$(pool_size "$p")" != "0" ]]; then running=1; fi
	done
	if [[ "$running" == "0" && "$(sql_state)" == "STOPPED" ]]; then
		echo "already parked — nothing to do"
		exit 0
	fi

	# Saved before the first resize, and all of them in one call: a partial save
	# followed by a failed resize would leave `start` restoring some pools and
	# refusing on others.
	labels=""
	for p in "${POOLS[@]}"; do
		size="$(pool_size "$p")"
		if [[ "$size" == "0" ]]; then continue; fi
		labels="${labels:+${labels},}${LABEL_PREFIX}${p}=${size}"
	done
	if [[ -n "$labels" ]]; then
		echo "saving pool sizes on the cluster: ${labels}"
		g container clusters update "$CLUSTER" --location="$LOCATION" \
			--update-labels="$labels" --quiet
	fi

	for p in "${POOLS[@]}"; do
		if [[ "$(pool_size "$p")" == "0" ]]; then
			echo "pool ${p} already at zero"
			continue
		fi
		echo "resizing pool ${p} to zero (nodes are drained first)"
		g container clusters resize "$CLUSTER" --location="$LOCATION" \
			--node-pool="$p" --num-nodes=0 --quiet
	done

	if [[ "$(sql_state)" == "STOPPED" ]]; then
		echo "cloud sql ${SQL_INSTANCE} already stopped"
	else
		echo "stopping cloud sql ${SQL_INSTANCE}"
		g sql instances patch "$SQL_INSTANCE" --activation-policy=NEVER --quiet
	fi

	echo
	echo "parked. storage, the control plane and the LoadBalancers keep billing;"
	echo "the nodes, their disks and the database instance no longer do."
	;;

start)
	if [[ "$(sql_state)" == "STOPPED" ]]; then
		echo "starting cloud sql ${SQL_INSTANCE}"
		g sql instances patch "$SQL_INSTANCE" --activation-policy=ALWAYS --quiet
	else
		echo "cloud sql ${SQL_INSTANCE} already running"
	fi

	# Waited for explicitly rather than trusting the patch to have finished. The
	# patch returns when the operation is done; the instance answering queries is
	# a later moment, and it is that one the pods need.
	waited=0
	while [[ "$(sql_state)" != "RUNNABLE" ]]; do
		if [[ "$waited" -ge "$SQL_TIMEOUT" ]]; then
			echo "cloud sql did not reach RUNNABLE in ${SQL_TIMEOUT}s — pools left at zero" >&2
			echo "(re-run start once it does; nothing here is half-applied)" >&2
			exit 1
		fi
		sleep "$SQL_POLL"
		waited=$((waited + SQL_POLL))
	done
	echo "cloud sql is RUNNABLE after ${waited}s"

	restored=""
	for p in "${POOLS[@]}"; do
		current="$(pool_size "$p")"
		if [[ "$current" != "0" ]]; then
			echo "pool ${p} already at ${current}"
			continue
		fi
		size="$(saved_size "$p")"
		if [[ -z "$size" ]]; then
			size="$NODES"
		fi
		if [[ -z "$size" ]]; then
			echo "no saved size for pool ${p} and NODES is unset — refusing to guess" >&2
			echo "(this environment runs one node a pool; variables.tf defaults to two)" >&2
			echo "re-run as: NODES=1 PROJECT=${PROJECT} make gcp-env-start" >&2
			exit 1
		fi
		echo "restoring pool ${p} to ${size}"
		g container clusters resize "$CLUSTER" --location="$LOCATION" \
			--node-pool="$p" --num-nodes="$size" --quiet
		restored="${restored:+${restored},}${LABEL_PREFIX}${p}"
	done

	# Cleared only after every resize has succeeded, so an interrupted start can
	# simply be re-run: the labels are still there to restore from.
	if [[ -n "$restored" ]]; then
		g container clusters update "$CLUSTER" --location="$LOCATION" \
			--remove-labels="$restored" --quiet
	fi

	echo
	echo "running. pods reschedule themselves — nothing needs redeploying."
	echo "staging is at whatever commit it was parked on; to catch it up to main:"
	echo "    gh workflow run deploy.yml --ref main"
	;;
esac
