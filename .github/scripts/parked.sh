#!/usr/bin/env bash
# Is this cluster parked, as a pure function of its resource labels.
#
# `make gcp-env-stop` parks staging by writing `power-saved-<pool>=<n>` labels on
# the CLUSTER, and only `gcp-env-start` removes them. That is why the signal is
# the LABEL and not the node count: a cluster at zero nodes carrying no label was
# not parked by anyone, it fell over, and the two must never be confused.
#
# It lives in its own file because two workflows read it and they MUST agree.
# deploy.yml skips a deployment when this says `parked`; staging-parked.yml opens
# an issue when it says `parked`. Were they ever to disagree, one of them would
# be lying about the same cluster at the same moment — either an issue saying
# staging is parked while deploys install into it, or silence about one nothing
# has deployed to for a month. One file, one answer. `make parked-test` is the
# test, and it exists because neither caller can be exercised before merge.
#
# Usage: parked.sh <resource-labels>
#   resource-labels  gcloud's `value(resourceLabels)` rendering — `k=v;k=v`, or
#                    empty when the cluster carries no labels at all
#
# Prints exactly one word on stdout: `parked` or `live`.
set -euo pipefail

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <resource-labels>" >&2
	exit 2
fi

# What env-power.sh writes. The two are checked against each other in ci.yml,
# because a mismatch is silent in both directions: this file would stop seeing a
# parked cluster, or start seeing one that is running.
PREFIX="power-saved-"

# Anchored at a KEY, not matched anywhere in the string. `value()` renders the
# map as `key=value;key=value`, so a bare `*power-saved-*` would also match a
# VALUE that happens to contain the prefix — `deployment-note=power-saved-example`
# would report a cluster nobody parked. Prefixing the subject with the separator
# makes the first key follow a `;` like every other key does, and a value never
# can: a GCP label may hold only lowercase letters, digits, `-` and `_`.
case ";$1" in
	*";${PREFIX}"*) verdict=parked ;;
	*) verdict=live ;;
esac

printf '%s\n' "$verdict"
