- **Three package comments described shipped code as unbuilt** (#413) — `internal/worker` said its
  lease loop "is a later increment" although `cmd/worker` has driven it for months; `internal/queue`
  said "two kinds share the work_items table" where five do; `internal/egress` called the gate "a
  later slice" while the gate constructs its engine today. Each is rewritten from the code, and the
  queue's dividing line is now stated correctly: `Claim` is the in-process lane, `Poll` serves a BYOC
  worker exactly one thing — a `tool_exec` item on a `self_hosted` environment, never a `model_turn`
  row. Two documented lists had drifted the same way and are corrected: the wire ID prefixes (`outc_`
  and `skillver_` were missing) and the live-tier table, which omitted `RUN_LIVE_KMS_TESTS`.
