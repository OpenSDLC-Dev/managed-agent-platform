#!/bin/sh
set -eu
# CONSOLE_AUTH_MODE is the single routing switch, not an inference from the IdP.
case "${CONSOLE_AUTH_MODE:-}" in
  '')
    [ -z "${CONSOLE_PUBLIC_ORIGIN:-}" ] || { echo 'legacy routing cannot set CONSOLE_PUBLIC_ORIGIN' >&2; exit 1; }
    export APP_EDGE_MODE=legacy
    ;;
  platform)
    [ -n "${CONSOLE_PUBLIC_ORIGIN:-}" ] || { echo 'platform routing requires CONSOLE_PUBLIC_ORIGIN' >&2; exit 1; }
    case "${IDENTITY_MODE:-}" in
      password|oidc|trusted_proxy) ;;
      *) echo 'platform routing requires an enabled native identity mode' >&2; exit 1 ;;
    esac
    export APP_EDGE_MODE=native
    ;;
  *) echo 'CONSOLE_AUTH_MODE must be empty or platform' >&2; exit 1 ;;
esac
if [ -n "${MODELGATEWAY_UPSTREAM:-}" ]; then
  export APP_EDGE_ADMIN=enabled
else
  export APP_EDGE_ADMIN=disabled
fi
exec caddy run --config /etc/caddy/Caddyfile --adapter caddyfile
