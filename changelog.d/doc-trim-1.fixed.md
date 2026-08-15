- **Two documentation defects with teeth** — `changelog.d/cd-build-on-runner.fixed.md` did not
  begin with `- `, which the fragment loader rejects outright, so the next `make changelog` would
  have failed at assembly; it is a well-formed entry now. And `docs/deploy-gcp.md`'s sandbox-pool
  sizing still said nothing calls `Sandbox.Destroy`, so operators were told to size for
  cumulative rather than concurrent sessions — a reap loop has run in every executor since plan
  24, and the guidance now names what frees a pod and what weakens that: with no blob backend
  the idle tier is off, so an idle session holds its pod until it terminates, and a
  `self_hosted` sandbox belongs to the customer's worker and is never reaped. (#413)
