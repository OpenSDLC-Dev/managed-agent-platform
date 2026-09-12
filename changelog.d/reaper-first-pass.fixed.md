- **A restarting executor tears down its predecessor's sandboxes at once, not an interval
  later** — the sandbox reaper waited out a full `EXECUTOR_REAP_INTERVAL` before its first
  pass, because a Go ticker does not fire when it is created. It usually had a boot pass
  anyway, but not one of its own: the reap-kick listener sweeps whenever it establishes a
  `LISTEN`, and that wake was carrying it. Where the listener never establishes — a pooler
  that refuses `LISTEN` or multiplexes away the backend that would deliver it, a server with
  no connection to spare for the dial — an executor restarting more often than the interval
  left its predecessor's containers standing after every restart, and a crash-looping one left
  them for as long as it kept crashing. The loop now passes before it waits, as the five
  control-plane sweeps already do (#709).
