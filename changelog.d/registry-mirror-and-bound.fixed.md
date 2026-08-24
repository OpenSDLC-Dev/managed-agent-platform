- **The session-id prefix entry is an inference, not a mirror** — Its whole text in
  [docs/DIVERGENCES.md](./docs/DIVERGENCES.md) said the alternate `session_` form is accepted
  "mirroring the reference's dual form", which nothing ever observed: the claim dates to this
  repo's first commit and its citations resolve to this repo's own wire rule. Every concrete
  session id in the pinned SDK, the `ant` CLI and the public sessions pages is `sesn_`; the only
  reference-side `session_` is the work-data schema's description of `data.id`, which the SDK
  worker feeds straight back into session paths. So the entry moves to INFERRED under #78 rather
  than to the architecture notes, keeps the lenient parse, and says what a recording would
  settle. (#463)
- **Four registry entries name the recording that would settle them** — The management-key
  `expires_at` entry records a 1–128-character `name` bound it calls unobserved and carried no
  tracker at all. Its `can_manage` twin (falsy case unobserved), the mixed-permission entry
  (reference behavior unspecified) and the web-tools entry (`citations.enabled`) had the same
  gap, the last of them behind a closed issue. Each now points at #78 with a parenthetical
  naming what it leaves open — a gap `registrycheck` cannot see, since it demands a pointer
  only under the INFERRED heading. (#465)
