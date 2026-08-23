- **Two converged registry entries say which divergence they are the record of** — The
  skills-`latest` entry and the management-key `expires_at` entry in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) both read as mirrors of the reference, which made
  them look mis-sectioned under the test #450 wrote into the CONFIRMED heading. Both are in fact
  converged, and each now opens by saying so. The skills entry's divergence was never the version
  normalization, which always matched: it was the deferral behind the field — no `/v1/skills` API,
  no storage, no materialization, no injection — and the sentence naming it had been dropped when
  skills slice 5 closed it, leaving "The deferral is now closed" with nothing to refer back to and
  a title advertising the match. The `expires_at` entry's was its refusal of an instant already
  past, which #389 measured at `200` on the reference and lifted. (#458)
