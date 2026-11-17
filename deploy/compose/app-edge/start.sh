#!/bin/sh
set -eu
[ -n "${CONSOLE_PUBLIC_ORIGIN:-}" ] || { echo 'routing requires CONSOLE_PUBLIC_ORIGIN' >&2; exit 1; }
case "${IDENTITY_MODE:-}" in
  password|oidc|trusted_proxy) ;;
  *) echo 'routing requires an enabled native identity mode' >&2; exit 1 ;;
esac
if [ -n "${MODELGATEWAY_UPSTREAM:-}" ]; then
  export APP_EDGE_ADMIN=enabled
else
  export APP_EDGE_ADMIN=disabled
fi
exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
