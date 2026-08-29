- **`make identifiers-test` now holds that rule, instead of memory** (#514). It
  searches the documentation for the two shapes nothing there may legitimately carry
  — a routable IPv4 address and a bare 12-digit project number — and runs in CI. A
  guard cannot search for the values themselves without containing them, which is
  the thing being prevented. Its self-test runs first on every invocation, because a
  broken pattern and a clean repository otherwise print the same thing. Scope is
  markdown under `docs/` and `deploy/`; the source tree is deliberately left out,
  where every networking test is full of addresses on purpose.
