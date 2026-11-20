#!/bin/sh
# Structured read-only inspection interface for the issue-triage subagent
# (.claude/agents/issue-triage.md). Enforces a narrow subcommand and argument
# allowlist and executes git/gh directly via exec with external diff/textconv
# and pagers disabled — never eval, never shell re-parsing (#1052).
#
# Usage:
#   .claude/hooks/issue-triage-read.sh [--check] <subcommand> [args...]
#
# Subcommands:
#   issue-view <number> [--comments]
#   issue-list [--state open|closed|all] [--limit <1-100>] [--label <label>]...
#   pr-view <number> [--comments]
#   git-log [-n <1-200>] [--oneline] [--stat] [--name-only] [<rev>] [-- <relpath>...]
#   git-show [--stat] [--name-only] [--no-patch] <rev> [-- <relpath>...]
set -eu
set -f

die() {
  printf 'issue-triage-read: %s\n' "$1" >&2
  exit 2
}

is_uint_in_range() {
  val=$1
  min_v=$2
  max_v=$3
  case "$val" in
    ''|*[!0-9]*) return 1 ;;
  esac
  # Strip leading zeros safely before arithmetic comparison (avoid octal).
  trimmed=$(printf '%s' "$val" | sed 's/^0*//')
  [ -n "$trimmed" ] || trimmed=0
  [ "$trimmed" -ge "$min_v" ] && [ "$trimmed" -le "$max_v" ]
}

check_rev() {
  rev=$1
  case "$rev" in
    ''|-*|*[!A-Za-z0-9._/^~:@-]*)
      die "invalid git revision: $rev" ;;
  esac
}

check_rel_path() {
  p=$1
  case "$p" in
    ''|-*|/*|..|../*|*/..|*/../*|*[!A-Za-z0-9._/@+-]*)
      die "invalid repository-relative path: $p" ;;
  esac
}

check_label() {
  lbl=$1
  case "$lbl" in
    ''|-*|*[!A-Za-z0-9._:/+-]*)
      die "invalid label: $lbl" ;;
  esac
}

check_only=0
if [ $# -gt 0 ] && [ "$1" = "--check" ]; then
  check_only=1
  shift
fi

[ $# -gt 0 ] || die "subcommand is required (issue-view, issue-list, pr-view, git-log, git-show)"
subcmd=$1
shift

case "$subcmd" in
  issue-view|pr-view)
    [ $# -ge 1 ] || die "$subcmd requires a numeric id"
    num=$1
    shift
    is_uint_in_range "$num" 1 999999999 || die "$subcmd id must be a positive integer"
    comments=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --comments)
          comments=1
          shift ;;
        *)
          die "$subcmd does not allow option or argument: $1" ;;
      esac
    done
    [ "$check_only" = 0 ] || exit 0
    if [ "$subcmd" = "issue-view" ]; then
      if [ "$comments" = 1 ]; then
        GH_PAGER=cat PAGER=cat exec gh issue view "$num" --comments
      fi
      GH_PAGER=cat PAGER=cat exec gh issue view "$num"
    else
      if [ "$comments" = 1 ]; then
        GH_PAGER=cat PAGER=cat exec gh pr view "$num" --comments
      fi
      GH_PAGER=cat PAGER=cat exec gh pr view "$num"
    fi
    ;;

  issue-list)
    state=''
    limit=''
    labels_count=0
    # Store validated labels as newline-free safe tokens separated by space
    # (check_label forbids spaces and shell metacharacters, and set -f is on).
    labels=''
    while [ $# -gt 0 ]; do
      case "$1" in
        --state)
          [ $# -ge 2 ] || die "--state requires a value"
          case "$2" in
            open|closed|all) state=$2 ;;
            *) die "--state must be open, closed, or all" ;;
          esac
          shift 2 ;;
        --limit)
          [ $# -ge 2 ] || die "--limit requires a value"
          is_uint_in_range "$2" 1 100 || die "--limit must be an integer between 1 and 100"
          limit=$2
          shift 2 ;;
        --label)
          [ $# -ge 2 ] || die "--label requires a value"
          check_label "$2"
          labels_count=$((labels_count + 1))
          [ "$labels_count" -le 10 ] || die "at most 10 --label filters are allowed"
          if [ -z "$labels" ]; then
            labels=$2
          else
            labels="$labels $2"
          fi
          shift 2 ;;
        *)
          die "issue-list does not allow option or argument: $1" ;;
      esac
    done
    [ "$check_only" = 0 ] || exit 0
    set -- issue list
    if [ -n "$state" ]; then
      set -- "$@" --state "$state"
    fi
    if [ -n "$limit" ]; then
      set -- "$@" --limit "$limit"
    fi
    if [ -n "$labels" ]; then
      for lbl in $labels; do
        set -- "$@" --label "$lbl"
      done
    fi
    GH_PAGER=cat PAGER=cat exec gh "$@"
    ;;

  git-log)
    count=''
    oneline=0
    stat=0
    name_only=0
    rev=''
    has_paths=0
    while [ $# -gt 0 ]; do
      case "$1" in
        -n)
          [ $# -ge 2 ] || die "-n requires a positive integer"
          is_uint_in_range "$2" 1 200 || die "-n must be an integer between 1 and 200"
          count=$2
          shift 2 ;;
        --oneline)
          oneline=1
          shift ;;
        --stat)
          stat=1
          shift ;;
        --name-only)
          name_only=1
          shift ;;
        --)
          shift
          has_paths=1
          break ;;
        -*)
          die "git-log does not allow option: $1" ;;
        *)
          [ -z "$rev" ] || die "git-log accepts at most one revision before --"
          check_rev "$1"
          rev=$1
          shift ;;
      esac
    done
    if [ "$has_paths" = 1 ]; then
      [ $# -gt 0 ] || die "git-log requires at least one path after --"
      for p in "$@"; do
        check_rel_path "$p"
      done
      set -- -- "$@"
    fi
    if [ -n "$rev" ]; then
      set -- "$rev" "$@"
    fi
    if [ "$name_only" = 1 ]; then
      set -- --name-only "$@"
    fi
    if [ "$stat" = 1 ]; then
      set -- --stat "$@"
    fi
    if [ "$oneline" = 1 ]; then
      set -- --oneline "$@"
    fi
    if [ -n "$count" ]; then
      set -- -n "$count" "$@"
    fi
    [ "$check_only" = 0 ] || exit 0
    exec git --no-pager \
      -c core.fsmonitor=false \
      -c core.pager=cat \
      -c diff.external= \
      -c diff.textconv= \
      log --no-ext-diff --no-textconv "$@"
    ;;

  git-show)
    stat=0
    name_only=0
    no_patch=0
    rev=''
    has_paths=0
    while [ $# -gt 0 ]; do
      case "$1" in
        --stat)
          stat=1
          shift ;;
        --name-only)
          name_only=1
          shift ;;
        --no-patch)
          no_patch=1
          shift ;;
        --)
          shift
          has_paths=1
          break ;;
        -*)
          die "git-show does not allow option: $1" ;;
        *)
          [ -z "$rev" ] || die "git-show accepts at most one revision before --"
          check_rev "$1"
          rev=$1
          shift ;;
      esac
    done
    [ -n "$rev" ] || die "git-show requires a revision"
    if [ "$has_paths" = 1 ]; then
      [ $# -gt 0 ] || die "git-show requires at least one path after --"
      for p in "$@"; do
        check_rel_path "$p"
      done
      set -- -- "$@"
    fi
    set -- "$rev" "$@"
    if [ "$no_patch" = 1 ]; then
      set -- --no-patch "$@"
    fi
    if [ "$name_only" = 1 ]; then
      set -- --name-only "$@"
    fi
    if [ "$stat" = 1 ]; then
      set -- --stat "$@"
    fi
    [ "$check_only" = 0 ] || exit 0
    exec git --no-pager \
      -c core.fsmonitor=false \
      -c core.pager=cat \
      -c diff.external= \
      -c diff.textconv= \
      show --no-ext-diff --no-textconv "$@"
    ;;

  *)
    die "unsupported subcommand: $subcmd (allowed: issue-view, issue-list, pr-view, git-log, git-show)"
    ;;
esac
