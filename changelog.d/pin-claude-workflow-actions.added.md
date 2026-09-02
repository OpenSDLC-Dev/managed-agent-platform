- **`make pins-test` now checks that every action a workflow runs is pinned to a commit SHA**
  (#518), and runs in CI. `.github/dependabot.yml` has stated that rule since #96 and nothing in
  the repository could notice when #472 put four mutable tags back, so this checks the shape: a
  40-character commit SHA, and a trailing `# vX.Y.Z` naming the release — not because Dependabot
  needs one, but because forty hex characters say nothing about what runs, so without it the diff
  that moves an action is unreviewable. Shape is all it checks and its header says so: whether a
  SHA *is* the release beside it needs the network, which a gate has not got. Local actions and
  `docker://` images are exempt for want of a SHA. A `uses:` it cannot place stops the run by name
  and line rather than passing silently, and its self-test runs first, because a broken pattern
  and a clean repository otherwise print the same thing.
