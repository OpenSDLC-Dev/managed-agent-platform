# checkpoint-shell template — the only bash this tool introduces, embedded once
# as data. Go substitutes __STATE__, __ID__ and __SNAP__ (all single-quoted, all
# platform paths and ids with no shell metacharacters) before this runs as the
# exec's /bin/bash -c argument. The user's command is NOT here: it is delivered as
# a file and sourced, so no command bytes ride the argument or a sentinel, and the
# command runs as this exec's OWN process — the sandbox's outside-the-container
# deadline applies to it exactly as to a plain Exec, and cannot be forged from
# inside.
#
# Continuity comes from a snapshot written into a directory of this call's own,
# which __map_save finishes by creating a `done` marker. The caller commits the
# snapshot — by pointing `head` at it — only when the call finished inside its
# deadline AND that marker is there. Three things follow. A timed-out call's
# mutations are dropped. A save that never ran or never finished — the shell was
# replaced by `exec`, killed outright, or exited through an EXIT trap of the
# command's own — is not committed either, so `head` keeps pointing at the last
# snapshot that IS complete instead of at an empty or half-written one. And a
# command the sandbox abandoned cannot hand its state to a later call by writing
# its snapshot long after Exec stopped waiting: that write lands in a directory
# nothing will ever point at.
#
# Every variable and function this template owns is named __map_*, and __map_*
# names are excluded from the snapshot — exports, functions AND aliases alike — so
# a command that exports `__map_state` or defines `__map_save` cannot reach across
# into the next call's machinery.
__map_state=__STATE__
__map_id=__ID__
__map_snap=__SNAP__
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
#
# Every one of those builtins is invoked through `builtin`, because the save runs
# in the SAME shell as the command and a bash function overrides a builtin of the
# same name. A command that defines `printf() { return 0; }` — a wrapper, not an
# attack — would otherwise have the save write an empty `names` file, report
# success, and earn its marker; the next call would then restore `env` and unset
# every name not listed in `names`, i.e. all of them, leaving the shell without so
# much as a PATH. `case`, `while` and `[[` are keywords and cannot be overridden,
# so the control flow needs no such guard.
__map_save() {
  builtin local __map_code=${1:-$?} __map_opts_ok=0 __map_body_ok=0

  # The command's traps go first, before this function does anything else. Traps
  # do not carry (see the divergences), so dropping them costs nothing — and
  # keeping them costs the whole snapshot. Under `set -E` or `set -T` an ERR or
  # DEBUG trap is inherited into the process substitutions below, and a trap that
  # prints to STDOUT writes straight into the pipe the `while read` loop is
  # consuming: its text is read as if it were a variable name, `declare -p` on
  # that garbage fails, errexit aborts the save, and the call loses everything it
  # did. `set -E; trap 'echo ...' ERR` is an ordinary script idiom, not sabotage.
  builtin trap - DEBUG ERR RETURN

  # Options are captured first, and in THIS shell. Two traps, both of which cost
  # `set -e` its persistence: capturing after the `set +e` below records
  # `set +o errexit` every time, and capturing through a command substitution
  # records it too — a subshell does not inherit errexit unless inherit_errexit
  # is set, which by default it is not. The `||` keeps a failed write from
  # aborting the save while errexit is still on — but it RECORDS that failure
  # rather than swallowing it, because a snapshot missing its options is an
  # incomplete snapshot and must not be committed.
  { builtin shopt -p; builtin set +o; } >"$__map_snap/opts" 2>/dev/null || __map_opts_ok=1

  # Alias expansion is switched off for the rest of the save — AFTER the options
  # are captured, so the snapshot still records whether the command had it on.
  # `builtin` guards this function's own words against a shadowing function, but
  # it cannot guard them against an alias, because a function body is parsed once
  # at definition time while a COMMAND OR PROCESS SUBSTITUTION INSIDE IT IS
  # RE-PARSED EVERY TIME IT RUNS. So `alias builtin=true` — carried, since
  # `expand_aliases` is an option and options carry — reaches into the already-
  # parsed body below and turns every `< <(builtin compgen …)` into
  # `< <(true compgen …)`: the loops read nothing, `names`, `env`, `funcs` and
  # `aliases` are all written empty, every write "succeeds", the marker is earned,
  # and the snapshot commits. The next call then restores that empty `env` and
  # unsets every name the empty `names` does not list — which is all of them,
  # PATH included. Aliases are already captured and do not carry out of here, so
  # dropping their expansion costs nothing.
  builtin shopt -u expand_aliases

  # xtrace goes off with errexit, and only after the options are captured, so a
  # command that ran under `set -x` still carries it. This trims the trace the
  # save would otherwise spill into the tool result's stderr; it does not remove
  # it. A handful of lines — __map_main's own, and the two above — are traced
  # before this runs, and a command that asked for `set -x` gets them.
  builtin set +ex
  # errexit inside the subshell is what makes a failing write abort the rest, and
  # the subshell's exit status is what gates the marker. The subshell has to be a
  # command in its own right, its status read from `$?`: bash IGNORES errexit
  # inside a compound command that is the left-hand side of `&&` or `||` — even an
  # explicit `set -e` within it — so writing this as `( set -e; ... ) && : >done`
  # would let a middle write fail, let the writes after it succeed, and create the
  # marker anyway. The subshell is a fork, so pwd/exports/functions/aliases are
  # the parent's.
  (
    builtin set -e
    builtin pwd >"$__map_snap/cwd"

    # One `declare -p` per exported name, never a line filter over `declare -px`:
    # a line-oriented filter cuts the interior lines of a multi-line value in half
    # and leaves an unterminated quote behind. SHELLOPTS and BASHOPTS are readonly
    # in every fresh bash and cannot be re-declared, so they are the only names
    # dropped — every other readonly export is carried as one. `names` records
    # what the snapshot carries, so the restore can tell a variable the command
    # unset from one that was never there.
    builtin : >"$__map_snap/env"
    builtin : >"$__map_snap/names"
    while IFS= builtin read -r __map_n; do
      case "$__map_n" in SHELLOPTS | BASHOPTS | __map_*) continue ;; esac
      builtin printf '%s\n' "$__map_n" >>"$__map_snap/names"
      builtin declare -p "$__map_n" >>"$__map_snap/env"
    done < <(builtin compgen -e)

    # Functions, one at a time and minus our own: a command that defines
    # __map_save would otherwise have it restored over ours on the next call.
    builtin : >"$__map_snap/funcs"
    while IFS= builtin read -r __map_n; do
      case "$__map_n" in __map_*) continue ;; esac
      builtin declare -f "$__map_n" >>"$__map_snap/funcs"
    done < <(builtin compgen -A function)

    # Aliases, one at a time and filtered the same way — NOT a bare `alias -p`.
    # The template's own trailing lines are parsed after the restore has sourced
    # this file, so an alias that survives into the next call is expanded over
    # them: `alias trap=true` alone turned `trap '__map_save' EXIT` into a no-op
    # and silently dropped the state of every call that ended by calling `exit`,
    # for the rest of the session. __map_main below closes that from the other
    # side; the filter keeps a command from aliasing the template's own names.
    #
    # Each record is written as `\builtin alias name='value'`, because the restore
    # sources this file with the command's own FUNCTIONS already live: a command
    # that defines `alias() { :; }` would otherwise have every line of it read as
    # a call to that function, and the session would lose its alias table for
    # good. The leading backslash is not decoration — a quoted word is never
    # alias-expanded — and the prefix rides in front of the whole record rather
    # than each line, so an alias whose value spans lines stays one command.
    builtin : >"$__map_snap/aliases"
    while IFS= builtin read -r __map_n; do
      case "$__map_n" in __map_*) continue ;; esac
      builtin printf '\\builtin ' >>"$__map_snap/aliases"
      builtin alias "$__map_n" >>"$__map_snap/aliases"
    done < <(builtin compgen -a)
  ) >/dev/null 2>&1
  __map_body_ok=$?

  # The marker, last and only if every write above succeeded — the options in this
  # shell and the body in the subshell. It is what the caller checks before it
  # commits, so an incomplete snapshot is simply never pointed at.
  if [[ $__map_opts_ok -eq 0 && $__map_body_ok -eq 0 ]]; then
    builtin : >"$__map_snap/done" 2>/dev/null
  fi

  builtin return $__map_code
}

# Everything that runs AFTER the restore lives in this function, and that is the
# point: a function body is parsed when it is DEFINED — here, before the restore
# has sourced a single alias — so the plain words below cannot be alias-expanded.
# Left at the top level they are parsed after the restore, where `alias trap=true`
# turns the EXIT trap into a no-op and `alias exit=true` costs the call its exit
# code. `builtin` guards the same words against a snapshotted *function* of the
# same name, which the restore has already installed by then. (Being inside a
# function body is no protection for a command or process substitution: those are
# re-parsed every time they run, which is what __map_save must switch alias
# expansion off for. There are none here.)
#
# The EXIT trap catches a command that calls `exit`; the explicit save after it
# catches a command that returns normally having replaced the EXIT trap with one
# of its own. Clearing the EXIT trap first keeps an untouched one from saving
# twice. Clearing DEBUG/ERR/RETURN here does not protect the snapshot — __map_save
# clears them again on entry, and that is the clear that matters, because a command
# exiting THROUGH the EXIT trap never reaches this line. This one protects the tool
# RESULT: without it, each line of the template below fires the command's own DEBUG
# or ERR trap, and the model reads output its command never printed.
#
# What is left is narrow and documented: a command that BOTH installs its own EXIT
# trap AND exits through it skips this call's snapshot — and so, therefore, does
# one that replaces this shell with `exec` or has it killed outright. None of them
# lose more than their own call: an uncommitted snapshot leaves `head` alone.
__map_main() {
  builtin trap '__map_save' EXIT
  builtin . "$__map_state/cmd/$__map_id"
  __map_status=$?
  builtin trap - DEBUG ERR RETURN EXIT
  __map_save "$__map_status"
  builtin exit "$__map_status"
}

# Restore the committed snapshot (the first call, and the first call after a
# restart, have none). The `done` marker is the judge of whether there is one:
# a directory alone is not, because every call creates its own the moment it
# starts. errexit is forced off for the prologue so a snapshot that turned it on
# cannot abort the restore; options are applied last so the command still runs
# under them. With no snapshot to restore, the shell simply stays where the exec
# put it — the sandbox's own workdir — rather than assuming a path.
#
# This whole group is parsed as one compound command before any of it runs, so the
# aliases it sources cannot be expanded over its own later lines. Its words still
# go through `builtin`, because parsing is no defence against a shadowing
# FUNCTION: the moment the group sources `funcs`, the command's own definitions
# are live, over the rest of the group and over the files it has yet to source.
# `[[` and `case` are keywords and cannot be shadowed at all.
set +e
{
  __map_head=$(cat "$__map_state/head" 2>/dev/null)
  __map_prev="$__map_state/snap/$__map_head"
  if [[ -n "$__map_head" && -f "$__map_prev/done" ]]; then
    builtin cd "$(cat "$__map_prev/cwd" 2>/dev/null)" 2>/dev/null || :
    [[ -f "$__map_prev/env" ]] && builtin . "$__map_prev/env"

    # This exec is a fresh bash, so it re-inherits the container's own environment:
    # a variable the shell UNSET would silently reappear unless it is removed
    # again. Every word below is a builtin, because the diff has to survive a
    # snapshot in which the shell unset PATH itself. The names are read a line at
    # a time under `IFS=`, never split out of `$(compgen -e)`: the env restored
    # just above may carry an IFS of the command's own, and an exported `IFS=`
    # stops that splitting entirely — the whole diff then collapses to one
    # nonsense word, nothing is unset, and an unset secret quietly comes back.
    if [[ -f "$__map_prev/names" ]]; then
      # No `=()` initializer: bash parses a compound assignment only when
      # `declare` is the literal command word, so `builtin declare -A x=()` is a
      # syntax error. The exec is a fresh shell and `__map_*` is never restored,
      # so there is nothing here to clear anyway.
      builtin declare -A __map_keep
      while IFS= builtin read -r __map_n; do __map_keep["$__map_n"]=1; done <"$__map_prev/names"
      while IFS= builtin read -r __map_n; do
        case "$__map_n" in __map_*) continue ;; esac
        [[ -n "${__map_keep[$__map_n]+x}" ]] || builtin unset "$__map_n"
      done < <(builtin compgen -e)
      builtin unset __map_keep __map_n
    fi

    # From here the command's own functions are live — `funcs` puts them there —
    # so the words this group runs, and the words the files it sources run, are
    # all reachable by a function of the same name. `.` and `[` above all: both
    # are legal function names, both are snapshotted like any other, and a
    # `[() { return 1; }` alone made every line below it a no-op.
    [[ -f "$__map_prev/funcs" ]] && builtin . "$__map_prev/funcs"
    [[ -f "$__map_prev/aliases" ]] && builtin . "$__map_prev/aliases"

    # Options are applied a line at a time through `builtin` rather than sourced,
    # because sourcing runs the file's OWN words — `set -o pipefail`, `shopt -s
    # nullglob` — in a shell where `set` and `shopt` may now be a function or an
    # alias of the command's own. `set() { :; }` cost the session every option it
    # had, silently and permanently: the restore dropped them, the save then
    # snapshotted a shell that no longer had them, and every later call carried
    # the loss forward. Unlike the rest of the shadowing family, that one fails
    # UNSAFE — it earns its marker and commits.
    #
    # The eval'd text is bash's own `shopt -p` / `set +o` output, one option per
    # line, which sourcing already executed verbatim; the eval adds no input the
    # file did not already have, only the `\builtin` in front of it. The
    # backslash is what an alias cannot get past: `eval` re-parses, and by this
    # point an `alias builtin=true` of the command's own may be live — but a
    # quoted word is never alias-expanded.
    if [[ -f "$__map_prev/opts" ]]; then
      while IFS= builtin read -r __map_l; do
        builtin eval "\\builtin $__map_l"
      done <"$__map_prev/opts"
    fi
  fi
  builtin unset __map_head __map_prev __map_l
} >/dev/null 2>&1

__map_main
