# checkpoint-shell template — the only bash this tool introduces, embedded once
# as data. Go substitutes __STATE__ and __ID__ (both single-quoted, both platform
# ids with no shell metacharacters) before this runs as the exec's /bin/bash -c
# argument. The user's command is NOT here: it is delivered as a file and sourced,
# so no command bytes ride the argument or a sentinel, and the command runs as
# this exec's OWN process — the sandbox's outside-the-container deadline applies
# to it exactly as to a plain Exec, and cannot be forged from inside.
#
# Continuity comes from a snapshot written into THIS call's own directory. The
# caller commits it — by pointing `head` at it — only when the call finished
# inside its deadline. So a timed-out call's mutations are dropped, and a command
# the sandbox abandoned cannot hand its state to a later call by writing its
# snapshot long after Exec stopped waiting: that write lands in an id-scoped
# directory nothing will ever point at.
#
# Every variable and function this template owns is named __map_*, and __map_*
# names are excluded from the snapshot, so a command that exports `__map_state`
# or defines `__map_save` cannot reach across it into the next call.
__map_state=__STATE__
__map_id=__ID__
__map_snap="$__map_state/snap/$__map_id"
mkdir -p "$__map_snap" >/dev/null 2>&1

# __map_save snapshots the shell, preserving the command's own exit status: an
# EXIT trap that does not itself call exit leaves the process status untouched,
# so the writes below never change what the exec reports. Each file is written to
# a temp name and renamed, so a reader never sees a half-write — and a SIGKILL
# landing mid-save leaves the snapshot merely uncommitted rather than torn.
__map_save() {
  local code=${1:-$?}

  # Options are captured first, and in THIS shell. Two traps, both of which cost
  # `set -e` its persistence: capturing after the `set +e` below records
  # `set +o errexit` every time, and capturing through a command substitution
  # records it too — a subshell does not inherit errexit unless inherit_errexit
  # is set, which by default it is not. `|| true` keeps a failed write from
  # aborting the save while errexit is still on.
  { shopt -p; set +o; } >"$__map_snap/opts.t" 2>/dev/null || true

  set +e
  {
    pwd >"$__map_snap/cwd.t" && mv -f "$__map_snap/cwd.t" "$__map_snap/cwd"

    # One `declare -p` per exported name, never a line filter over `declare -px`:
    # a line-oriented filter cuts the interior lines of a multi-line value in half
    # and leaves an unterminated quote behind. SHELLOPTS and BASHOPTS are readonly
    # in every fresh bash and cannot be re-declared, so they are the only names
    # dropped — every other readonly export is carried as one. `names` records
    # what the snapshot carries, so the restore can tell a variable the command
    # unset from one that was never there.
    : >"$__map_snap/env.t"
    : >"$__map_snap/names.t"
    local __map_n
    while IFS= read -r __map_n; do
      case "$__map_n" in SHELLOPTS | BASHOPTS | __map_*) continue ;; esac
      printf '%s\n' "$__map_n" >>"$__map_snap/names.t"
      declare -p "$__map_n" >>"$__map_snap/env.t"
    done < <(compgen -e)
    mv -f "$__map_snap/env.t" "$__map_snap/env"
    mv -f "$__map_snap/names.t" "$__map_snap/names"

    # Functions, one at a time and minus our own: a command that defines
    # __map_save would otherwise have it restored over ours on the next call.
    : >"$__map_snap/funcs.t"
    while IFS= read -r __map_n; do
      case "$__map_n" in __map_*) continue ;; esac
      declare -f "$__map_n" >>"$__map_snap/funcs.t"
    done < <(compgen -A function)
    mv -f "$__map_snap/funcs.t" "$__map_snap/funcs"

    alias -p >"$__map_snap/aliases.t" && mv -f "$__map_snap/aliases.t" "$__map_snap/aliases"

    mv -f "$__map_snap/opts.t" "$__map_snap/opts"
  } >/dev/null 2>&1
  return $code
}

# Restore the committed snapshot (the first call, and the first call after a
# restart, have none). errexit is forced off for the prologue so a snapshot that
# turned it on cannot abort the restore; options are applied last so the command
# still runs under them.
set +e
{
  __map_head=$(cat "$__map_state/head" 2>/dev/null)
  __map_prev="$__map_state/snap/$__map_head"
  if [ -n "$__map_head" ] && [ -d "$__map_prev" ]; then
    cd "$(cat "$__map_prev/cwd" 2>/dev/null)" 2>/dev/null || cd /workspace
    [ -f "$__map_prev/env" ] && . "$__map_prev/env"

    # This exec is a fresh bash, so it re-inherits the container's own environment:
    # a variable the shell UNSET would silently reappear unless it is removed
    # again. Every word below is a builtin, because the diff has to survive a
    # snapshot in which the shell unset PATH itself.
    if [ -f "$__map_prev/names" ]; then
      declare -A __map_keep=()
      while IFS= read -r __map_n; do __map_keep["$__map_n"]=1; done <"$__map_prev/names"
      for __map_n in $(compgen -e); do
        case "$__map_n" in __map_*) continue ;; esac
        [ -n "${__map_keep[$__map_n]+x}" ] || unset "$__map_n"
      done
      unset __map_keep __map_n
    fi

    [ -f "$__map_prev/funcs" ] && . "$__map_prev/funcs"
    [ -f "$__map_prev/aliases" ] && . "$__map_prev/aliases"
    [ -f "$__map_prev/opts" ] && . "$__map_prev/opts"
  else
    cd /workspace
  fi
} >/dev/null 2>&1
unset __map_head __map_prev

# The EXIT trap catches a command that calls `exit`; the explicit save after it
# catches a command that returns normally having replaced the EXIT trap with one
# of its own. Clearing the trap first keeps an untouched one from saving twice.
# What is left is narrow and documented: a command that BOTH installs its own EXIT
# trap AND exits through it skips this call's snapshot.
trap '__map_save' EXIT
. "$__map_state/cmd/$__map_id"
__map_status=$?
trap - EXIT
__map_save "$__map_status"
exit "$__map_status"
