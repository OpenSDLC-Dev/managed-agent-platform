- **Memory versions no longer accumulate forever** (#476) — the control plane now
  sweeps hourly for versions past the reference's 30-day window that are not among
  their memory's newest **five**, the last of plan 36's known consequences that was
  a job to write rather than a decision to make: a chatty agent rewriting a 100 kB
  memory every turn had been adding 100 kB per turn to a table nothing ever pruned.
  The window is the reference's, published; the count is this platform's, because
  the reference says "the recent versions are always kept" without saying how many.
  A live memory's head is never swept. A deleted memory's history prunes by the
  same rule as any other, which leaves it listable rather than immortal, and so
  does a redacted version. A store's hard delete still takes everything at once.
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) carries the count and what it rests on.
