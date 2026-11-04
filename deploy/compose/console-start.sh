#!/bin/sh
set -eu
case "${CONSOLE_AUTH_MODE:-}" in
  '') ;;
  platform)
    if [ -n "${PLATFORM_API_KEY:-}${CONSOLE_PASSWORD:-}${IDENTITY_MODE:-}" ]; then
      echo 'native Console must not receive legacy credentials or IdP configuration' >&2
      exit 1
    fi
    ;;
  *) echo 'CONSOLE_AUTH_MODE must be empty or platform' >&2; exit 1 ;;
esac
exec node server.js
