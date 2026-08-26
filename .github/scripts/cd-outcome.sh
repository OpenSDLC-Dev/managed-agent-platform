#!/usr/bin/env bash
# What a finished `deploy` run means, as a pure function of three strings.
#
# It lives in its own file rather than inline in deploy-alert.yml because a
# `workflow_run` workflow cannot be exercised before it is merged — it runs the
# copy on the default branch — so inline logic would first execute against real
# issues on `main`. `make cd-outcome-test` is the test.
#
# Usage: cd-outcome.sh <run-conclusion> <deploy-step-conclusion> <steps-ran>
#   run-conclusion         the run's own conclusion, verbatim from the API
#   deploy-step-conclusion the "Deploy the chart" step's conclusion, or `absent`
#   steps-ran              `true` if ANY step in the run reached a conclusion
#
# Prints exactly one word on stdout.
set -euo pipefail

if [ "$#" -ne 3 ]; then
	echo "usage: $0 <run-conclusion> <deploy-step-conclusion> <steps-ran>" >&2
	exit 2
fi

run="$1"
step="$2"
ran="$3"

case "$run" in
	success)
		case "$step" in
			# The release was installed.
			success) outcome=deployed ;;
			# The parked check short-circuited everything after it: green, and
			# nothing was deployed. deploy.yml does this deliberately so that a
			# money-saving weekend does not read as an outage.
			skipped) outcome=parked ;;
			# The step this classifier keys on is not in the run — renamed,
			# removed, or split. This must be LOUD: as the quiet arm it would
			# turn every healthy deploy into "parked" and no successful run
			# would ever close an issue again.
			*) outcome=miswired ;;
		esac ;;
	# Defensive, not descriptive: today's deploy.yml cannot produce this — its
	# job carries no `if:` and its non-main guard FAILS rather than skipping. It
	# is here because a run whose every job skipped deployed nothing and broke
	# nothing, and the arm that must never be the quiet default is `*`.
	skipped) outcome=noop ;;
	cancelled)
		# Two very different events share this conclusion. deploy.yml queues on
		# `concurrency: deploy-staging`, and GitHub cancels the PENDING run in a
		# group when a newer one queues — so back-to-back merges routinely
		# produce a run that executed nothing. Reporting that as a CD failure is
		# the kind of false alarm that teaches people to mute the notifier.
		#
		# A run cancelled MID-FLIGHT is the opposite: deploy.yml's own
		# concurrency comment calls it the state that wedges a release halfway
		# through `helm upgrade`, with no run left to finish it. That must alert.
		#
		# Whether any step reached a conclusion is what separates them.
		if [ "$ran" = "true" ]; then outcome=broken; else outcome=superseded; fi ;;
	*) outcome=broken ;;
esac

printf '%s\n' "$outcome"
