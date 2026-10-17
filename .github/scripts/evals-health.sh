#!/usr/bin/env bash
# Whether the nightly eval suite is passing, as a pure function of the evals
# workflow's run history and the time.
#
# It lives in its own file rather than inline in evals-alert.yml because a
# scheduled workflow cannot be run from a pull request: its first execution is
# against real issues on `main`. `make evals-alert-test` is the test.
#
# Usage: evals-health.sh <now> <stale-hours> < attempts.json
#   now            the current time, in seconds since the epoch
#   stale-hours    how old the newest verdict may be and still count
#   attempts.json  one array of run attempts on main: the runs endpoint's
#                  `workflow_runs`, each its latest attempt, plus the earlier
#                  attempts of every re-run from the attempts endpoint
#
# Prints one line: <state> <run-id> <attempt> <conclusion> <finished>. The state
# is healthy, failing or stale; the rest names the newest verdict, or is `-`
# four times when there is none.
set -euo pipefail

usage() {
	echo "usage: $0 <now-epoch-seconds> <stale-hours> < attempts.json" >&2
	exit 2
}

[ "$#" -eq 2 ] || usage
case "$1" in '' | *[!0-9]*) usage ;; esac
case "$2" in '' | *[!0-9]*) usage ;; esac
[ "$2" -gt 0 ] || usage

# A verdict is an attempt that says whether the suite passed. `cancelled` does
# not: a pending run evicted from the `evals` concurrency group by a newer one
# ends that way without executing, and so do a hand cancel and a job that hit
# its timeout. Neither do `skipped`, `neutral`, `action_required` or `stale`,
# and an attempt still in progress has no conclusion yet. Those attempts are
# passed over, and the staleness clock is what notices when nothing but them
# arrives.
#
# The newest verdict is the one with the highest `run_number`, which is the
# newest code, and within a run the highest `run_attempt`. Not the one that
# finished last: a re-run of an older run tests that run's older commit, and
# must not outvote a newer run. Not `created_at` either: the runs endpoint gives
# a run's own, and the attempts endpoint gives when each attempt began.
#
# Staleness is checked first. A verdict older than the limit means nothing has
# run since it, and that is the newer news.
#
# `-n` with `input`, because plain `jq` given no input at all prints nothing and
# exits 0, and the caller would read that as no answer rather than as a failure.
# The limit is computed in jq, whose numbers do not overflow as bash's do.
jq -nr --argjson now "$1" --arg hours "$2" '
	input
	| if type != "array" then error("expected one array of run attempts, got \(type)") else . end
	| if ([inputs] | length) > 0 then error("expected one JSON value") else . end
	| ($hours | tonumber * 3600) as $limit
	| map(select(.conclusion == "success" or .conclusion == "failure"
	             or .conclusion == "timed_out" or .conclusion == "startup_failure"))
	| sort_by(.run_number, .run_attempt) | last
	| if . == null then "stale - - - -"
	  else (if $now - (.updated_at | fromdateiso8601) > $limit then "stale"
	        elif .conclusion == "success" then "healthy"
	        else "failing" end)
	       + " \(.id) \(.run_attempt) \(.conclusion) \(.updated_at)"
	  end'
