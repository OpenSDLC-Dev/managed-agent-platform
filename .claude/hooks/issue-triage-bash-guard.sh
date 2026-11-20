#!/bin/sh
# PreToolUse guard for the issue-triage subagent (.claude/agents/issue-triage.md).
# The agent is judgment-only and read-only; its frontmatter cannot express a Bash
# command allowlist (the tools field takes bare names only), so this hook restricts
# Bash to the structured read-only wrapper (.claude/hooks/issue-triage-read.sh)
# with zero shell metacharacters, quotes, escapes, globs, or expansions, and
# validates the exact post-split tokens via `issue-triage-read.sh --check` (#1052).
# Stdin is the PreToolUse JSON payload; tool_input.command is the Bash command.
set -eu
set -f
LC_ALL=C
export LC_ALL

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -- "$script_dir/../.." && pwd -P)
reader="$script_dir/issue-triage-read.sh"

# The parse step must not depend on a working python3 on PATH: the Microsoft
# Store's alias stub exits without reading stdin, which turned this line's
# old `|| exit 2` into a blanket deny before the allowlist ever ran (#295).
# Each interpreter is probed with a known-good document first, so a parse
# failure on the real payload keeps meaning "malformed payload — deny" and
# never falls through to the fallback. Only when no working interpreter
# exists does the POSIX extraction run, and it fails closed on any value it
# cannot decode faithfully — a backslash in the raw match is either an
# escape it would have to interpret or a truncated match at an escaped
# quote — so the allowlist itself never judges a mis-parsed command.
payload=$(cat)
json='import json,sys
data = json.load(sys.stdin)
if not isinstance(data, dict):
    sys.exit(1)
ti = data.get("tool_input")
if not isinstance(ti, dict):
    sys.exit(1)
cmd = ti.get("command", "")
if not isinstance(cmd, str):
    sys.exit(1)
# Reject NUL and line breaks inside Python before command substitution can
# truncate at NUL (ksh), drop NUL (bash/dash), or strip trailing newlines.
if "\x00" in cmd or "\n" in cmd or "\r" in cmd:
    sys.exit(3)
sys.stdout.write(cmd + "x")'
py=''
for p in python3 python; do
  if printf '{}' | "$p" -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
    py=$p
    break
  fi
done
if [ -n "$py" ]; then
  rc=0
  cmd_marked=$(printf '%s' "$payload" | "$py" -c "$json" 2>/dev/null) || rc=$?
  if [ "$rc" = 3 ]; then
    echo "issue-triage guard: command contains disallowed NUL or newline characters" >&2
    exit 2
  elif [ "$rc" != 0 ]; then
    echo "issue-triage guard: the PreToolUse payload is not valid JSON" >&2
    exit 2
  fi
  cmd=${cmd_marked%x}
else
  # The extraction must be unambiguous as well as faithful: sed's greedy
  # match selects the LAST "command" key, and the harness executes
  # tool_input.command — so a payload carrying anything but exactly one
  # bare "command" needle is denied rather than judged by the wrong key.
  # The NF pattern guard matters: awk sets NF=0 on a blank record, so an
  # unguarded `c += NF - 1` would subtract one per blank line and let a
  # two-key payload with a blank line count as one.
  needles=$(printf '%s' "$payload" | awk -F'"command"' 'NF { c += NF - 1 } END { print c + 0 }')
  if [ "$needles" != 1 ]; then
    echo "issue-triage guard: no working python on PATH and the payload does not carry exactly one \"command\" key, so the command cannot be attributed faithfully" >&2
    exit 2
  fi
  cmd=$(printf '%s' "$payload" | sed -n 's/.*"command"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
  case "$cmd" in
    *\\*)
      echo "issue-triage guard: no working python on PATH and the command carries JSON escapes; rephrase it without quotes, backslashes, or control characters" >&2
      exit 2;;
  esac
fi

# Reject empty commands and ANY character outside a narrow ASCII
# metacharacter-free allowlist (LC_ALL=C pins character classes to ASCII).
case "$cmd" in
  ''|*[!abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._/:@~^+' '-]*)
    echo "issue-triage guard: command is empty or contains disallowed characters or shell metacharacters" >&2
    exit 2;;
esac

# Deny leading whitespace and tilde-expansion at the start of any token
# (mid-token ~ such as HEAD~1 never expands in POSIX sh/bash).
case "$cmd" in
  ' '*|~*|*' ~'*)
    echo "issue-triage guard: leading whitespace or tilde expansion is not allowed" >&2
    exit 2;;
esac

# Split strictly on ASCII space (set -f disabled globbing above, and no quotes,
# escapes, tabs, or newlines survived the character allowlist).
IFS=' '
# shellcheck disable=SC2086
set -- $cmd

# Require direct invocation of the structured read-only wrapper from the
# repository root so the relative path executed by Bash is guaranteed to be
# the exact validated reader beside this hook (not a shadowed copy in a
# subdirectory).
if [ $# -lt 2 ] || [ "$1" != ".claude/hooks/issue-triage-read.sh" ]; then
  echo "issue-triage guard: only '.claude/hooks/issue-triage-read.sh <subcommand> ...' is allowed (read-only triage)" >&2
  exit 2
fi
if [ "$(pwd -P)" != "$repo_root" ]; then
  echo "issue-triage guard: read-only helper must be invoked from the repository root" >&2
  exit 2
fi
shift

# Validate the exact subcommand and argument allowlist using the wrapper itself
# so hook and wrapper enforce the identical boundary.
"$reader" --check "$@"
