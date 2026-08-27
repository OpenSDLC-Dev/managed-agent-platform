- **The CD notifier's retry no longer folds a failed attempt's output into the answer**
  (#507). `deploy-alert.yml` wraps its four GitHub API reads in a three-attempt `retry`, and
  every caller captures what that helper prints — but it ran each attempt with stdout already
  wired to the capture, so bytes emitted before a failure stayed there and the caller parsed
  them joined to the attempt that worked: a truncated body became unparseable JSON, and the
  open-issue lookup returned `123123` where it meant "nothing open". Each attempt is now
  buffered and only the successful one printed, matching the copy `staging-parked.yml` already
  carried. The two helpers stay deliberately separate — two notifiers retrying differently
  harms nobody, unlike the parked-cluster rule they do share — so `make retry-test` lifts each
  one out of its workflow and runs it against a flaky command, and neither can drift back
  alone.
