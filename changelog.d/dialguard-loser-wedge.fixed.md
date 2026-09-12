- **A `dialguard` test no longer risks holding a CI run for its package's whole
  30-minute timeout** (#689). `TestTheLosingFamilysConnectionIsClosed` waited on an
  unbounded channel for a connection the losing address family produces only if its
  dial was entered at all — which a fallback winning first can prevent. One `coverage`
  job was lost to it, on a diff with no path to the package. The losing family's entry
  into the dial is now waited for rather than assumed, and every wait in the test is
  bounded, so a starved run fails in seconds rather than taking the whole run down
  with it. No other test in the package waits on a value handoff a cancellation can
  skip. Test-only; no production code changed.
