- **A restart no longer loses a scheduled run, or fails a dream that never started** — the
  deployment scheduler and the dream runner each waited out a full tick before their first
  pass, so a restart cost the work that fell in that window rather than delaying it: an
  occurrence that was still inside the one-hour catch-up window when the control plane booted
  could be outside it one tick later, never fired and recorded nowhere, and a dream left
  pending with less than a tick of its timeout budget was failed as `timeout` without ever
  starting. Both loops now take a pass before the first wait, as the memory, expired-file and
  object-delete sweeps already did — normally, since the dream runner's pass still yields when
  the shared sweep budget is saturated (#699).
