#!/bin/sh
# PreToolUse guard for the issue-triage subagent (.claude/agents/issue-triage.md).
# The agent is judgment-only and read-only; its frontmatter cannot express a Bash
# command allowlist (the tools field takes bare names only), so this hook enforces
# one: allow read-only gh/git inspection commands, deny everything else (exit 2).
# Stdin is the PreToolUse JSON payload; tool_input.command is the Bash command.

cmd=$(python3 -c 'import json,sys; print(json.load(sys.stdin).get("tool_input",{}).get("command",""))') || exit 2

case "$cmd" in
  ''|*';'*|*'|'*|*'&'*|*'>'*|*'<'*|*'`'*|*'$('*|*$'\n'*)
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
