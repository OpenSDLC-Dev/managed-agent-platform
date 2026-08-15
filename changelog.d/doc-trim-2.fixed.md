- **Twelve comments described shipped code as unbuilt, or claimed more than it does** (#413) —
  `internal/worker` said its lease loop "is a later increment" although `cmd/worker` has driven it
  for months, and `internal/queue` said "two kinds share the work_items table" where five do. Eight
  more still pointed at "a later slice" that has since landed: the gate that builds the egress
  substitution engine, the resolution that fills it, `Spec.Env`'s vault placeholders, four sites in
  `internal/identity` waiting on a principals table `upsertPrincipal` writes today, and the fake
  OpenID provider's own package doc. Two others overstated an invariant: the API's credential map
  called every remaining `/v1` route management-only, though the work API takes the environment key
  and nothing else, and `Spec.Image` claimed every mismatch is refused with `ErrSpecMismatch`,
  though a wedged gated pod is reclaimed first, deliberately. Each is rewritten from the code, and
  the queue's dividing line is now stated correctly: `Claim` is the in-process lane, `Poll` serves a
  BYOC worker exactly one thing — a `tool_exec` item on a `self_hosted` environment, never a
  `model_turn` row.
