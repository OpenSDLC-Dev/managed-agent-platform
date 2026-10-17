#!/usr/bin/env bash
# Whether the nightly eval suite is passing, as a pure function of the evals
# workflow's run history and the time.
#
# It lives in its own file rather than inline in evals-alert.yml because a
# scheduled workflow cannot be run from a pull request: its first execution is
# against real issues on `main`. `make evals-alert-test` is the test.
#
# Usage: evals-health.sh <now> <stale-hours> < runs.json
#   now          the current time, in seconds since the epoch
#   stale-hours  how old the newest verdict may be and still count
#   runs.json    the `workflow_runs` array of the runs endpoint, completed
#                runs on main
#
# Prints one line: <state> <run-id> <attempt> <conclusion> <finished>. The state
# is healthy, failing or stale; the rest names the newest run that reached a
# verdict, or is `-` four times when none did.
set -euo pipefail

usage() {
	echo "usage: $0 <now-epoch-seconds> <stale-hours> < runs.json" >&2
	exit 2
}

[ "$#" -eq 2 ] || usage
case "$1" in '' | *[!0-9]*) usage ;; esac
case "$2" in '' | *[!0-9]*) usage ;; esac
[ "$2" -gt 0 ] || usage

# A verdict is a run that says whether the suite passed. `cancelled` does not: a
# pending run evicted from the `evals` concurrency group by a newer one ends
# that way without executing, and so does a hand cancel. Neither do `skipped`,
# `neutral`, `action_required` or `stale`. Those runs are passed over, and the
# staleness clock is what notices when nothing but them arrives.
#
# The newest verdict is the newest `updated_at`, not the first in the list. The
# endpoint lists runs in creation order, and a re-run keeps its run's place
# while `updated_at` moves to when the new attempt ended.
#
# Staleness is checked before the verdict. A failure older than the limit means
# nothing has run since it, and that is the newer news.
#
# `-n` with `input`, because plain `jq` given no input at all prints nothing and
# exits 0, and the caller would read that as no answer rather than as a failure.
jq -nr --argjson now "$1" --argjson limit "$((10#$2 * 3600))" '
	input
	| map(select(.conclusion == "success" or .conclusion == "failure"
	             or .conclusion == "timed_out" or .conclusion == "startup_failure"))
	| sort_by(.updated_at) | last
	| if . == null then "stale - - - -"
	  else (if $now - (.updated_at | fromdateiso8601) > $limit then "stale"
	        elif .conclusion == "success" then "healthy"
	        else "failing" end)
	       + " \(.id) \(.run_attempt) \(.conclusion) \(.updated_at)"
	  end'
