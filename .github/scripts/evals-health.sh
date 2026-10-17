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
#   runs.json    the `workflow_runs` array of the runs endpoint for main, in
#                any status
#
# Prints one line: <state> <run-id> <attempt> <conclusion> <finished>. The state
# is healthy, failing, stale or pending; the rest names the run the state rests
# on, or is `-` four times when there is none.
set -euo pipefail

usage() {
	echo "usage: $0 <now-epoch-seconds> <stale-hours> < runs.json" >&2
	exit 2
}

[ "$#" -eq 2 ] || usage
case "$1" in '' | *[!0-9]*) usage ;; esac
case "$2" in '' | *[!0-9]*) usage ;; esac
[ "$2" -gt 0 ] || usage

# A verdict is a run whose latest attempt says whether the suite passed.
# `cancelled` does not: a pending run evicted from the `evals` concurrency group
# by a newer one ends that way without executing, and so do a hand cancel and a
# job that hit its timeout. Neither do `skipped`, `neutral`, `action_required`
# or `stale`, and a run still in progress has no conclusion yet. Those runs are
# passed over, and the staleness clock is what notices when nothing but them
# arrives.
#
# The newest run is the newest CREATED, which is the newest code: a re-run of
# an older run tests that run's older commit, and must not outvote a newer run.
#
# A re-run hides its earlier attempt: while the new attempt is in flight the run
# has no conclusion, and if it is cancelled its conclusion is `cancelled`. The
# earlier attempt may have failed, so passing over such a run could fall back to
# an older pass. It answers `pending` instead, and the caller changes nothing.
#
# Staleness is checked first. A verdict older than the limit means nothing has
# run since it, and that is the newer news.
#
# `-n` with `input`, because plain `jq` given no input at all prints nothing and
# exits 0, and the caller would read that as no answer rather than as a failure.
# The limit is computed in jq, whose numbers do not overflow as bash's do.
jq -nr --argjson now "$1" --arg hours "$2" '
	def verdict: .conclusion == "success" or .conclusion == "failure"
		or .conclusion == "timed_out" or .conclusion == "startup_failure";
	input
	| if type != "array" then error("expected the workflow_runs array, got \(type)") else . end
	| if ([inputs] | length) > 0 then error("expected one JSON value") else . end
	| ($hours | tonumber * 3600) as $limit
	| map(select(verdict or .run_attempt > 1))
	| sort_by(.created_at) | last
	| if . == null then "stale - - - -"
	  else (if $now - (.updated_at | fromdateiso8601) > $limit then "stale"
	        elif verdict | not then "pending"
	        elif .conclusion == "success" then "healthy"
	        else "failing" end)
	       + " \(.id) \(.run_attempt) \(.conclusion) \(.updated_at)"
	  end'
