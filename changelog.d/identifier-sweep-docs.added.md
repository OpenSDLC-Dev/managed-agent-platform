- **`make identifiers-test` now checks the documentation for the shapes an operator's
  coordinates take** (#514), and runs in CI. A guard cannot search for the values
  themselves without containing them, which is the thing being prevented, so it searches
  for four shapes: a routable IPv4 address, a bare 12-digit project number, and an
  Artifact Registry path or `*.iam.gserviceaccount.com` address whose project component is
  not one of this repository's placeholders. It is **not** a proof that the repository is
  clean — a bare project id in running text has no shape to match, and source files are
  outside the scope on purpose. Its self-test runs first on every invocation, because a
  broken pattern and a clean repository otherwise print the same thing.
