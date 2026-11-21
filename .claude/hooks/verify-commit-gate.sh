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
# Stdin is the Claude Code PreToolUse JSON payload.
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

payload="$(cat)"
if [ -z "$payload" ]; then
  exit 0
fi

# Parse PreToolUse payload and classify whether tool_input.command actually
# invokes `git ... commit`, plus whether it carries an explicit `Verified-By:`
# trailer. Uses python3 when available; falls back cleanly to exit 0 if no
# working python is available (never wedge environments without python).
py=""
for candidate in python3 python; do
  if printf '{}' | "$candidate" -c 'import json, sys; json.load(sys.stdin)' 2>/dev/null; then
    py="$candidate"
    break
  fi
done

if [ -z "$py" ]; then
  exit 0
fi

analysis="$(
  printf '%s' "$payload" | "$py" -c '
import json, re, shlex, sys

try:
    data = json.load(sys.stdin)
except Exception:
    print("SKIP")
    sys.exit(0)

if not isinstance(data, dict):
    print("SKIP")
    sys.exit(0)

ti = data.get("tool_input")
if not isinstance(ti, dict):
    print("SKIP")
    sys.exit(0)

cmd = ti.get("command", "")
if not isinstance(cmd, str) or not cmd.strip():
    print("SKIP")
    sys.exit(0)

session_id = data.get("session_id", "default")
if not isinstance(session_id, str) or not session_id:
    session_id = "default"
session_id = re.sub(r"[^A-Za-z0-9_.-]+", "_", session_id)[:64]

# Split command into shell segments on &&, ||, ;, |, &, or newlines and check
# whether any segment executes `git ... commit` (not inside echo/grep/comments).
def is_git_commit(command: str) -> bool:
    try:
        lex = shlex.shlex(command, posix=True, punctuation_chars="();<>|&\n")
        lex.whitespace = " \t\r"
        lex.whitespace_split = True
        lex.commenters = "#"
        tokens = list(lex)
    except ValueError:
        # Unclosed quotes/heredocs: fall back to line-aware regex check.
        return bool(re.search(r"(^|[;&|\n])\s*(?:[A-Za-z_][A-Za-z0-9_]*=\S*\s+)*git(?:\s+-[A-Za-z](?:\s+\S+)?|\s+--[a-z-]+(?:=\S+)?)*\s+commit(\s|$)", command))

    seps = {"&&", "||", ";", "|", "&", "\n"}
    segments = []
    cur = []
    for tok in tokens:
        if tok in seps:
            if cur:
                segments.append(cur)
                cur = []
        else:
            cur.append(tok)
    if cur:
        segments.append(cur)

    # Options to `git` before the subcommand that take a separate argument.
    takes_arg = {"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--config-env"}
    for seg in segments:
        i = 0
        # Skip leading env assignments like FOO=bar
        while i < len(seg) and re.match(r"^[A-Za-z_][A-Za-z0-9_]*=", seg[i]):
            i += 1
        if i >= len(seg):
            continue
        prog = seg[i].rsplit("/", 1)[-1]
        if prog != "git":
            continue
        i += 1
        while i < len(seg) and seg[i].startswith("-"):
            opt = seg[i]
            i += 1
            if opt in takes_arg and i < len(seg):
                i += 1
        if i < len(seg) and seg[i] == "commit":
            return True
    return False

if not is_git_commit(cmd):
    print("SKIP")
    sys.exit(0)

has_trailer = bool(re.search(r"(?im)(?:^|\n|--trailer(?:=|\s+)[\"'\'']?)Verified-By:\s*\S+", cmd))
print(f"COMMIT\t{session_id}\t{1 if has_trailer else 0}")
'
)"

case "$analysis" in
  SKIP|"")
    exit 0
    ;;
esac

IFS="$(printf '\t')" read -r _tag session_id has_trailer <<EOF
$analysis
EOF

if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  exit 0
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

if [ -f "$stamp_file" ]; then
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
