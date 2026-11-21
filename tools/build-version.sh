#!/bin/sh
# Resolve the source build before Docker or Cloud Build strips .git from the context.
set -eu
cd "$(dirname "$0")/.."

# An archive unpacked inside another repository cannot borrow its identity.
if [ "$(git rev-parse --show-prefix 2>/dev/null)" = "" ] && revision=$(git rev-parse --verify HEAD 2>/dev/null); then
    base=$(git for-each-ref --merged="$revision" --sort=-version:refname \
        --format='%(refname:strip=2)' 'refs/tags/v[0-9]*' |
        awk '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ && !found {print; found=1}')
    if [ -n "$base" ]; then
        printf '%s-dev-%.8s\n' "$base" "$revision"
        exit 0
    fi
fi
printf 'unknown\n'
