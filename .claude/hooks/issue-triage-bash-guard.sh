#!/bin/sh
# PreToolUse guard for the issue-triage subagent (.claude/agents/issue-triage.md).
# The agent is judgment-only and read-only; its frontmatter cannot express a Bash
# command allowlist (the tools field takes bare names only), so this hook enforces
# one: allow read-only gh/git inspection commands, deny everything else (exit 2).
# Stdin is the PreToolUse JSON payload; tool_input.command is the Bash command.

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
json='import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("command",""))'
py=''
for p in python3 python; do
  if printf '{}' | "$p" -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
    py=$p
    break
  fi
done
if [ -n "$py" ]; then
  cmd=$(printf '%s' "$payload" | "$py" -c "$json" 2>/dev/null) || {
    echo "issue-triage guard: the PreToolUse payload is not valid JSON" >&2
    exit 2
  }
else
  # The extraction must be unambiguous as well as faithful: sed's greedy
  # match selects the LAST "command" key, and the harness executes
  # tool_input.command — so a payload carrying anything but exactly one
  # bare "command" needle is denied rather than judged by the wrong key.
  # (Unreachable today — string values escape their quotes, and the Bash
  # tool_input has a single command key — but payload shapes drift.)
  needles=$(printf '%s' "$payload" | awk -F'"command"' '{ c += NF - 1 } END { print c + 0 }')
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

# Newline and carriage return are held in variables, not glob literals: under a
# POSIX /bin/sh (dash) the bashism $'\n' is the four literal characters $ ' \ n,
# so it would never match a real newline and an embedded second command would
# ride an allowed prefix. A literal-newline variable matches portably.
nl='
'
cr=$(printf '\r')
case "$cmd" in
  ''|*';'*|*'|'*|*'&'*|*'>'*|*'<'*|*'`'*|*'$('*|*"$nl"*|*"$cr"*)
    echo "issue-triage guard: empty command or shell metacharacters are not allowed" >&2
    exit 2;;
esac

# Write-capable or out-of-sandbox flags riding an allowed prefix: git's
# --output(-*) writes a file; gh's --web/-w opens a browser (-w is only denied
# for gh — for git log it is the whitespace flag and harmless).
case "$cmd" in
  'git '*)
    case " $cmd" in *' --output'*)
      echo "issue-triage guard: git --output writes a file and is not allowed" >&2
      exit 2;;
    esac;;
  'gh '*)
    case " $cmd" in *' --web'*|*' -w '*|*' -w')
      echo "issue-triage guard: gh --web/-w opens a browser and is not allowed" >&2
      exit 2;;
    esac;;
esac

case "$cmd" in
  'gh issue view '*|'gh issue list'|'gh issue list '*|'gh pr view '*|'git log'|'git log '*|'git show '*)
    exit 0;;
  *)
    echo "issue-triage guard: only 'gh issue view/list', 'gh pr view', 'git log', and 'git show' are allowed (read-only triage)" >&2
    exit 2;;
esac
