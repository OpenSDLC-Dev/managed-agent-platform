- **A slow lease renewal no longer abandons work whose lease is still live** (#392). Each renewal
  is bounded so it cannot outlive the lease it races, but the bound only held for a punctual tick:
  a renewal that overran its interval left the next attempt's deadline as much as a third of a
  lease *inside* the lease just bought. A slow database then failed the renewal with a
  deadline-exceeded error, cancelling a brain turn or an executor's tool run nothing else could
  yet claim. Each attempt is now budgeted from the last successful renewal's return; a tick
  arriving after that budget is spent reports the lease lost without dialing the database. A
  renewal timeout still does not prove the row free (#400).
