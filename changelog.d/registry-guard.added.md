- **The registry's pointer invariant is executable now, not asserted** — `Tracked: #N` in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) is a present-tense claim that work is outstanding,
  written once and falsified later by someone closing #N somewhere this repository cannot see;
  that is how 65 of 111 pointers went stale before anyone noticed. `tools/registrycheck`
  re-derives the fact instead of trusting it. Its shape rules — the clause grammar, an INFERRED
  entry with no live tracker, a tracker shared by several entries that says nothing about the one
  citing it, and a bare `(line NN)` cross-reference, which had already drifted 77 lines once —
  are offline and free, so they run inside `make verify` as that package's own test. The rule
  that needs GitHub cannot join a gate that is credential-free by design, so `make registry-check`
  carries it and a new scheduled workflow runs that daily and on every pull request touching the
  registry. Every rung is proved against a document mutated to break exactly it, because a guard
  that has never seen a broken file proves nothing. (#452)
