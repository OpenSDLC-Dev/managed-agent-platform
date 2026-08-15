- **Two documentation defects with teeth** — `changelog.d/cd-build-on-runner.fixed.md` did not
  begin with `- `, which the fragment loader rejects outright, so the next `make changelog` would
  have failed at assembly; it is a well-formed entry now. And `docs/deploy-gcp.md`'s sandbox-pool
  sizing still said nothing calls `Sandbox.Destroy`, so operators were told to size for
  cumulative rather than concurrent sessions — a reap loop has run in every executor since plan
  24, and the guidance now names what frees a pod, plus the two configurations where nothing
  does: an executor with no blob backend (the idle tier needs somewhere to checkpoint) and a
  `self_hosted` session, whose sandbox belongs to the customer's worker. (#413)
