# checkpoint-shell template — the only bash this tool introduces, embedded once
# as data. Go substitutes __STATE__ and __ID__ (both single-quoted, both platform
# ids with no shell metacharacters) before this runs as the exec's /bin/bash -c
# argument. The user's command is NOT here: it is delivered as a file and sourced,
# so no command bytes ride the argument or a sentinel, and the command runs as
# this exec's OWN process — the sandbox's outside-the-container deadline applies
# to it exactly as to a plain Exec, and cannot be forged from inside.
#
# Continuity comes from a snapshot written into THIS call's own directory, which
# __map_save finishes by creating a `done` marker. The caller commits the snapshot
# — by pointing `head` at it — only when the call finished inside its deadline AND
# that marker is there. Three things follow. A timed-out call's mutations are
# dropped. A save that never ran or never finished — the shell was replaced by
# `exec`, killed outright, or exited through an EXIT trap of the command's own —
# is not committed either, so `head` keeps pointing at the last snapshot that IS
# complete instead of at an empty or half-written one. And a command the sandbox
# abandoned cannot hand its state to a later call by writing its snapshot long
# after Exec stopped waiting: that write lands in an id-scoped directory nothing
# will ever point at.
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
# so the writes below never change what the exec reports.
#
# Every word that writes the snapshot is a BUILTIN — there is no `mv`, no external
# anything — so the save survives a command that broke PATH, exactly as the
# restore below already does. Atomicity comes from the `done` marker rather than
# from temp-file renames: the marker is created only if every write succeeded, and
# a snapshot without it is never committed, so a half-written one is never read.
__map_save() {
  local code=${1:-$?} __map_opts_ok=0 __map_body_ok=0

  # Options are captured first, and in THIS shell. Two traps, both of which cost
  # `set -e` its persistence: capturing after the `set +e` below records
  # `set +o errexit` every time, and capturing through a command substitution
  # records it too — a subshell does not inherit errexit unless inherit_errexit
  # is set, which by default it is not. The `||` keeps a failed write from
  # aborting the save while errexit is still on — but it RECORDS that failure
  # rather than swallowing it, because a snapshot missing its options is an
  # incomplete snapshot and must not be committed.
  { shopt -p; set +o; } >"$__map_snap/opts" 2>/dev/null || __map_opts_ok=1

  set +e
  # errexit inside the subshell is what makes a failing write abort the rest, and
  # the subshell's exit status is what gates the marker. The subshell has to be a
  # command in its own right, its status read from `$?`: bash IGNORES errexit
  # inside a compound command that is the left-hand side of `&&` or `||` — even an
  # explicit `set -e` within it — so writing this as `( set -e; ... ) && : >done`
  # would let a middle write fail, let the writes after it succeed, and create the
  # marker anyway. The subshell is a fork, so pwd/exports/functions/aliases are
  # the parent's.
  (
    set -e
    pwd >"$__map_snap/cwd"

    # One `declare -p` per exported name, never a line filter over `declare -px`:
    # a line-oriented filter cuts the interior lines of a multi-line value in half
    # and leaves an unterminated quote behind. SHELLOPTS and BASHOPTS are readonly
    # in every fresh bash and cannot be re-declared, so they are the only names
    # dropped — every other readonly export is carried as one. `names` records
    # what the snapshot carries, so the restore can tell a variable the command
    # unset from one that was never there.
    : >"$__map_snap/env"
    : >"$__map_snap/names"
    while IFS= read -r __map_n; do
      case "$__map_n" in SHELLOPTS | BASHOPTS | __map_*) continue ;; esac
      printf '%s\n' "$__map_n" >>"$__map_snap/names"
      declare -p "$__map_n" >>"$__map_snap/env"
    done < <(compgen -e)

    # Functions, one at a time and minus our own: a command that defines
    # __map_save would otherwise have it restored over ours on the next call.
    : >"$__map_snap/funcs"
    while IFS= read -r __map_n; do
      case "$__map_n" in __map_*) continue ;; esac
      declare -f "$__map_n" >>"$__map_snap/funcs"
    done < <(compgen -A function)

    alias -p >"$__map_snap/aliases"
  ) >/dev/null 2>&1
  __map_body_ok=$?

  # The marker, last and only if every write above succeeded — the options in this
  # shell and the body in the subshell. It is what the caller checks before it
  # commits, so an incomplete snapshot is simply never pointed at.
  if [ "$__map_opts_ok" -eq 0 ] && [ "$__map_body_ok" -eq 0 ]; then
    : >"$__map_snap/done" 2>/dev/null
  fi

  return $code
}

# Restore the committed snapshot (the first call, and the first call after a
# restart, have none). The `done` marker is the judge of whether there is one:
# a directory alone is not, because every call creates its own the moment it
# starts. errexit is forced off for the prologue so a snapshot that turned it on
# cannot abort the restore; options are applied last so the command still runs
# under them.
set +e
{
  __map_head=$(cat "$__map_state/head" 2>/dev/null)
  __map_prev="$__map_state/snap/$__map_head"
  if [ -n "$__map_head" ] && [ -f "$__map_prev/done" ]; then
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
# trap AND exits through it skips this call's snapshot — and so, therefore, does
# one that replaces this shell with `exec` or has it killed outright. None of them
# lose more than their own call: an uncommitted snapshot leaves `head` alone.
trap '__map_save' EXIT
. "$__map_state/cmd/$__map_id"
__map_status=$?
trap - EXIT
__map_save "$__map_status"
exit "$__map_status"
