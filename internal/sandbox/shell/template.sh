# checkpoint-shell template — the only bash the shell tool introduces, embedded
# once as data. Go substitutes __STATE__ and __ID__ (both quoted, and both are
# platform ids with no shell metacharacters) before this runs as the exec's
# /bin/bash -c argument. The user's command is NOT here: it is delivered as a
# file and sourced, so no command bytes ride the argument or a sentinel, and the
# command runs as this exec's OWN process — the sandbox's outside-the-container
# deadline applies to it exactly as to a plain Exec, and cannot be forged from
# inside. State (cwd, exported vars, functions, options, traps) is a checkpoint
# on the container's writable layer that the next call restores; a backgrounded
# process survives but the jobs table does not carry across calls.
STATE=__STATE__
id=__ID__
mkdir -p "$STATE/result" >/dev/null 2>&1

# Re-sourcing a saved environment must not abort on a declaration bash rejects:
# readonly variables (SHELLOPTS, BASHOPTS, and any the command marked readonly)
# cannot be reassigned, so drop every readonly declaration from the checkpoint.
__map_safe() { grep -v '^declare -[a-zA-Z]*r'; }

# __map_save checkpoints the shell on exit, preserving the command's own exit
# status: an EXIT trap that does not itself call exit leaves the process status
# untouched, so the writes below never change what the exec reports. Each file
# is written to a temp name and renamed so a reader never sees a half-write. A
# SIGKILL (a timeout) skips this trap, so a timed-out command's mutations are
# intentionally dropped.
__map_save() {
  local code=$?
  set +e
  {
    pwd                      >"$STATE/cwd.t"   && mv -f "$STATE/cwd.t"   "$STATE/cwd"
    declare -px | __map_safe >"$STATE/env.t"   && mv -f "$STATE/env.t"   "$STATE/env"
    declare -f               >"$STATE/funcs.t" && mv -f "$STATE/funcs.t" "$STATE/funcs"
    { shopt -p; set +o; }    >"$STATE/opts.t"  && mv -f "$STATE/opts.t"  "$STATE/opts"
    trap -p                  >"$STATE/traps.t" && mv -f "$STATE/traps.t" "$STATE/traps"
    printf '%s' "$code"      >"$STATE/result/$id"
  } >/dev/null 2>&1
  return $code
}

# Restore the prior checkpoint (the first call has none). errexit is forced off
# for the restore so a checkpoint that turned it on cannot abort the prologue;
# options are restored last so the command still runs under them.
set +e
{
  cd "$(cat "$STATE/cwd" 2>/dev/null)" 2>/dev/null || cd /workspace
  [ -f "$STATE/env" ]   && . "$STATE/env"
  [ -f "$STATE/funcs" ] && . "$STATE/funcs"
  [ -f "$STATE/traps" ] && . "$STATE/traps"
  [ -f "$STATE/opts" ]  && . "$STATE/opts"
} >/dev/null 2>&1

# Our checkpoint trap must win over any EXIT trap the checkpoint restored.
trap __map_save EXIT
. "$STATE/cmd/$id"
