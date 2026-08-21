#!/bin/sh
# One-time initialization + idempotent setup for the bundled OpenBao
# (docs/plan/12_vaults-credentials.md, D2). Runs as the compose openbao-init
# one-shot on every `compose up`:
#
#   1. `bao operator init` on first boot; the root token and recovery keys
#      land on the baoinit volume. Dev-grade by design — a self-unsealing
#      local stack necessarily stores its own bootstrap material. Production
#      uses an external OpenBao/Vault instead of this bundled instance.
#   2. Ensures the transit engine is mounted and the map-transit policy exists.
#   3. Mints/renews the deterministic platform token ($BAO_PLATFORM_TOKEN)
#      the controlplane and executor authenticate with: orphan, periodic,
#      scoped to transit only.
#
# Idempotent throughout: every step checks before it changes.
set -eu

# Everything this script creates holds bootstrap secrets (init.json carries
# the root token) — never group/world-readable, not even between creation and
# the explicit chmod below.
umask 077

INIT_FILE=/openbao/init/init.json

# The transit key name lands verbatim in the policy below; constrain it so a
# malformed value cannot mangle the policy document.
case "${BAO_TRANSIT_KEY:-}" in
'' | *[!a-zA-Z0-9_-]*)
	echo "BAO_TRANSIT_KEY must be non-empty and match [a-zA-Z0-9_-]" >&2
	exit 1
	;;
esac

initialized() {
	bao status -format=json 2>/dev/null | grep -q '"initialized": *true'
}

# Wait for the listener (the compose healthcheck already gates on it, but a
# race here would fail the whole stack).
tries=0
until bao status >/dev/null 2>&1 || [ $? -eq 2 ]; do
	tries=$((tries + 1))
	[ "$tries" -ge 60 ] && { echo "openbao never answered" >&2; exit 1; }
	sleep 1
done

if ! initialized; then
	echo "initializing openbao (first boot)"
	# A redirect truncates its target before the command runs, so initializing
	# straight into $INIT_FILE leaves an empty one behind when init fails —
	# which the guard below then has to tell apart from a lost volume (#439).
	# Land the output only once init has succeeded; whatever a failed init
	# left in the .tmp is the operator's to inspect, so nothing here sweeps it.
	bao operator init -format=json >"$INIT_FILE.tmp"
	mv "$INIT_FILE.tmp" "$INIT_FILE"
	chmod 600 "$INIT_FILE"
fi

# Missing, empty and part-written are one state to an operator and share one
# recovery, so the guard turns on the token the rest of this script needs
# rather than on the file holding it. No test of the file can stand in for
# that: `[ -f ]` walked straight past the 0-byte init.json a failed init used
# to leave (#439), `[ -s ]` walks past a truncated one, and `bao` writes
# root_token LAST — so the likeliest truncation of all lands inside the token,
# where even a test for the key would see one and read out nothing.
BAO_TOKEN=$(grep -o '"root_token": *"[^"]*"' "$INIT_FILE" 2>/dev/null | cut -d'"' -f4)
if [ -z "$BAO_TOKEN" ]; then
	echo "openbao is initialized but $INIT_FILE is missing or unusable (volume lost?);" >&2
	if [ -s "$INIT_FILE.tmp" ]; then
		echo "$INIT_FILE.tmp is not empty; check it for the root token before discarding anything" >&2
	fi
	echo "re-create the baodata+baoinit volumes together or initialize manually" >&2
	exit 1
fi
export BAO_TOKEN

if ! bao secrets list -format=json | grep -q '"transit/"'; then
	bao secrets enable transit
fi

# Scoped to the one transit key the platform uses — never a wildcard, so a
# leaked platform token cannot touch any other key this bao may grow.
bao policy write map-transit - <<EOF
path "transit/keys/${BAO_TRANSIT_KEY}" {
  capabilities = ["create", "read", "update"]
}
path "transit/encrypt/${BAO_TRANSIT_KEY}" {
  capabilities = ["update"]
}
path "transit/decrypt/${BAO_TRANSIT_KEY}" {
  capabilities = ["update"]
}
EOF

# The platform token is deterministic (compose injects the same value into the
# app services' BAO_TOKEN) and periodic; re-running this script on each
# `compose up` renews it, so an idle stack older than the period just needs a
# restart, not a re-init. A pre-existing token under this ID (an earlier
# manual init, a re-used ID) is adopted only if it carries the map-transit
# policy and not root; anything else fails closed.
if info=$(BAO_TOKEN="$BAO_PLATFORM_TOKEN" bao token lookup -format=json 2>/dev/null); then
	if ! echo "$info" | grep -q '"map-transit"' || echo "$info" | grep -q '"root"'; then
		echo "a token with the configured platform-token ID exists but does not carry the map-transit policy;" >&2
		echo "revoke it (BAO_TOKEN=<root> bao token revoke \$BAO_PLATFORM_TOKEN) or change BAO_PLATFORM_TOKEN" >&2
		exit 1
	fi
	BAO_TOKEN="$BAO_PLATFORM_TOKEN" bao token renew >/dev/null
else
	bao token create -id="$BAO_PLATFORM_TOKEN" -policy=map-transit -orphan -period=768h >/dev/null
fi
echo "openbao ready: transit mounted, platform token minted"
