#!/usr/bin/env bash
# PreToolUse Bash hook: verification commit gate (plan 75 slice 7).
#
# Intercepts `git commit` invocations and checks for either:
#   1. A fresh, matching worktree verification stamp (.git/.verify-stamp written
#      by `make verify`, `make test-fast`, `make test-race`, or verify-stamp.sh),
#   2. An explicit `Verified-By: <method|none>` trailer in the commit command.
#
# Rollout modes via VERIFY_GATE_MODE:
#   off      - disabled; allow immediately with no output
#   observe  - (default) log a warning on unverified commits and allow (exit 0)
#   enforce  - block unverified commits (exit 2) until stamped or trailered,
#              with an escape valve after VERIFY_GATE_MAX_BLOCKS (default 2)
#              consecutive blocks so an agent never wedges permanently.
#
# tools/harnesshook supplies validated session, trailer and worktree metadata.
set -euo pipefail
LC_ALL=C
export LC_ALL

mode="${VERIFY_GATE_MODE:-observe}"
case "$mode" in
  off)
    exit 0
    ;;
  observe|enforce)
    ;;
  *)
    echo "verify-commit-gate: unknown VERIFY_GATE_MODE=$mode; falling back to observe" >&2
    mode="observe"
    ;;
esac

max_age="${VERIFY_STAMP_MAX_AGE:-3600}"
max_blocks="${VERIFY_GATE_MAX_BLOCKS:-2}"
script_dir="$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)"
stamp_helper="$script_dir/verify-stamp.sh"

# Native event decoding and command classification are shared by the three clients.
if [ "$#" -ne 3 ]; then
  echo "usage: verify-commit-gate.sh SESSION TRAILER_FLAG known|unknown" >&2
  exit 2
fi
session_id="$1"
has_trailer="$2"
target="$3"

if [[ "$(git rev-parse --is-inside-work-tree 2>/dev/null || true)" != true ]]; then
  # The directory can exist before a compound command creates its repository.
  # The project owns the block counter while the target's identity is unknown.
  cd -- "$script_dir/../.."
  target=unknown
fi

git_dir_abs="$(CDPATH='' cd -- "$(git rev-parse --git-dir)" && pwd -P)"
state_file="${VERIFY_GATE_STATE_FILE:-${git_dir_abs}/.verify-gate-blocks-${session_id}}"

reset_blocks() {
  rm -f "$state_file" 2>/dev/null || true
}

read_blocks() {
  if [ -f "$state_file" ]; then
    local val
    val="$(cat "$state_file" 2>/dev/null || printf '0')"
    case "$val" in
      ''|*[!0-9]*) printf '0' ;;
      *) printf '%s' "$val" ;;
    esac
  else
    printf '0'
  fi
}

write_blocks() {
  printf '%s\n' "$1" > "$state_file"
}

# 1. Explicit Verified-By trailer satisfies the gate immediately.
if [ "$has_trailer" = "1" ]; then
  reset_blocks
  exit 0
fi

# 2. Check worktree verification stamp (.git/.verify-stamp).
stamp_file="$("$stamp_helper" --stamp-path)"
reason="no verification stamp found"
verified=0

if [ "$target" = unknown ]; then
  reason="the client did not identify the commit worktree"
elif [ -f "$stamp_file" ]; then
  stamp_ts=""
  stamp_method=""
  stamp_fp=""
  while IFS='=' read -r k v; do
    case "$k" in
      timestamp) stamp_ts="$v" ;;
      method) stamp_method="$v" ;;
      fingerprint) stamp_fp="$v" ;;
    esac
  done < "$stamp_file"

  now="$(date +%s)"
  cur_fp="$("$stamp_helper" --fingerprint)"

  if [ -z "$stamp_ts" ] || [ -z "$stamp_fp" ]; then
    reason="verification stamp is malformed"
  elif ! [[ "$stamp_ts" =~ ^[0-9]+$ ]] || [ $((now - stamp_ts)) -gt "$max_age" ]; then
    reason="verification stamp (${stamp_method:-unknown}) is older than ${max_age}s"
  elif [ "$stamp_fp" != "$cur_fp" ]; then
    reason="worktree changed since last verification (${stamp_method:-unknown})"
  elif "$stamp_helper" --check-partial-staging; then
    reason="staged index differs from verified working tree (partial staging)"
  else
    verified=1
  fi
fi

if [ "$verified" = "1" ]; then
  reset_blocks
  exit 0
fi

# 3. Unverified commit handling according to rollout mode.
if [ "$mode" = "observe" ]; then
  echo "verify-commit-gate [observe]: allowing unverified git commit ($reason). Run \`make test-fast\` / \`make verify\` or include a \`Verified-By: <method|none>\` trailer." >&2
  exit 0
fi

# mode == enforce: apply escape valve after max_blocks consecutive blocks.
blocks="$(read_blocks)"
if [ "$blocks" -ge "$max_blocks" ]; then
  reset_blocks
  echo "verify-commit-gate [enforce]: escape valve triggered after ${blocks} consecutive blocks ($reason) — allowing commit. Record \`Verified-By:\` and verify in CI." >&2
  exit 0
fi

next_blocks=$((blocks + 1))
write_blocks "$next_blocks"
echo "verify-commit-gate [enforce]: blocked unverified git commit ($reason) [block ${next_blocks}/${max_blocks}]. Run \`make test-fast\` / \`make verify\` (which updates .git/.verify-stamp) or add a \`Verified-By: <method|none>\` trailer to the commit message." >&2
exit 2
