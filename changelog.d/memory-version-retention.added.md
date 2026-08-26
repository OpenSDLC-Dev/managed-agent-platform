- **Memory versions no longer accumulate forever** (#476) — the control plane now
  sweeps hourly for versions past the reference's 30-day window that are not among
  their memory's newest **five**, the last plan-36 consequence left standing: a
  chatty agent rewriting a 100 kB memory every turn had been adding 100 kB per turn
  to a table nothing ever pruned. The window is the reference's, published; the
  count is this platform's, because retention is a server-side background policy
  and no recording of the reference can reveal the "recent versions" it says are
  "always kept" without saying how many — which is why plan 36 filed the number
  rather than guessed at it. A live memory's head is never swept, and a deleted
  memory's history prunes by the same rule as any other, which leaves it listable
  rather than immortal: the newest rows it keeps include the `deleted` version that
  ends it. A store's hard delete still takes everything at once. See
  [docs/DIVERGENCES.md](docs/DIVERGENCES.md) for the count and what it rests on.
