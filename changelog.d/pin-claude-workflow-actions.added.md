- **`make pins-test` now checks that every action a workflow runs is pinned to a commit SHA**
  (#518), and runs in CI. `.github/dependabot.yml` has stated that rule since #96 and nothing in
  the repository could notice when #472 put four mutable tags back, so this checks the shape: a
  40-character commit SHA, and a trailing `# vX.Y.Z` naming the release, without which Dependabot
  cannot refresh the pin. Shape is all it can check, and its header says so — whether a SHA *is*
  the release its comment names needs the network, which is Dependabot's half of the pair. Local
  actions and `docker://` images are exempt for want of a SHA to pin. A `uses:` it cannot place
  stops the run by name and line rather than passing silently, and its self-test runs first on
  every invocation, because a broken pattern and a clean repository otherwise print the same thing.
