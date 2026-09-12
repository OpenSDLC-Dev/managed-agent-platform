- **A restart no longer loses a scheduled run, or fails a dream that never started** — the
  deployment scheduler and the dream runner each waited out a full tick before their first
  pass, because a Go ticker does not fire when it is created. At thirty seconds that reads as
  latency, and for almost every occurrence it is, but each loop has a cutoff where the missed
  tick costs the work rather than delaying it. The scheduler's fire lookup is clamped to the
  one-hour catch-up window, so an occurrence that was still inside the window when the control
  plane booted can be outside it one tick later — never fired, and recorded nowhere; the
  deployment most exposed to that is the one restarting through an outage of about that length,
  which is what the window was sized for. The dream runner measures a dream's timeout from its
  creation and checks the timeout before the start, so a dream left pending with less than a
  tick of budget is failed as `timeout` by the pass that would otherwise have started it. Both
  loops now take one pass before the first wait, which is the order the memory, expired-file and
  object-delete sweeps already take; a pass at boot is safe on every replica at once for each
  loop's own existing reason — the scheduler claims an occurrence through a unique-index insert,
  and the runner claims a dream `FOR UPDATE SKIP LOCKED` (#699).
