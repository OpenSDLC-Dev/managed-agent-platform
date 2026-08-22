- **The registry's sections say what belongs in them** — `docs/DIVERGENCES.md`'s CONFIRMED and
  INFERRED headings carried no definition, so entries drifted into a section their own text
  denied. Both now state their test: a converged entry stays under CONFIRMED as the record of
  what once diverged, and an entry no recording could settle is not an inference. Under that
  rule the dial-address floor moves to CONFIRMED and the agent-toolset default policy — settled
  by the public permission-policies guide, not a recording — becomes an architecture note,
  while the two converged entries stay; #59, which asked for that recording, is closed. The
  long-poll `block_ms` entry regains the live pointer its two open readings had lost. (#450)
- **The session-status entry says what the public docs settle** — It claimed a session awaiting
  any tool stays `running`, and that `requires_action` serves confirmation gates alone. The
  first half is right and now says so; the second is wrong. The managed-agents docs make a
  custom-tool result a first-class blocker — the session parks at `session.status_idle` with
  `stop_reason.requires_action` — and say the same of a `self_hosted` worker's tool result. This
  platform leaves both `running`, so a client written to the published loop waits forever. That
  is a bug rather than a divergence, and the entry, with its cadence twin, now names #375,
  which owns it, not the catch-all recording tracker. (#451)
