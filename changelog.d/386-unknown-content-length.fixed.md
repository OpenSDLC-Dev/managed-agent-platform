- **A BYOC worker mounts its session's files even when the download declares no length**
  (#386). The worker asked the write seam for exactly as many bytes as the download's
  `Content-Length` promised, and passed that number on without looking at it. Go reports
  `-1` when there is no promise to read — a chunked response, or one a hop compressed and
  the transport decompressed on the way — and `-1` is not a count any stream can deliver, so
  the seam refused it, correctly, as a mismatch. Every file mount behind such an
  intermediary was therefore skipped: a tolerated per-file miss, logged and counted but
  never fatal, so the session started on time and the agent was handed a workspace with
  nothing in it — while being told the opposite, because the mounted-files block the brain
  puts in the system prompt is rendered from the session's own resource rows and says a
  mount "is available at the given path" whether or not any byte of it ever landed. Only the
  BYOC worker was exposed. The executor's twin takes its size from the object store, where a
  byte count always exists, and this platform's own control plane always sets the header —
  the `-1` arrives from whatever sits between the worker and it, which in a self-hosted
  deployment is the customer's network, not ours.
  The worker now measures what it is given: a download with no declared length is spooled to
  a temp file, which yields the count the seam requires, and the mount is written from the
  spool. To disk rather than to memory, because a mount may be 500 MB — and bounded, at the
  same 500 MB the Files API caps an upload at, since a body that never declares its end is
  also a body that need never end. An over-budget spool is refused rather than truncated
  into a silently short mount. The spool is unlinked the instant it exists, so a worker
  killed mid-mount — an eviction, an OOM, a rolling redeploy — strands none of a customer's
  file bytes on a disk this platform does not operate and would never sweep.
  What the measurement cannot do is check the transfer against anything, since the count now
  comes from the bytes that arrived. Two of the three ways a length goes missing carry their
  own end-of-message and fail loudly — chunked framing errors on a missing terminator, a
  decompressed body fails its checksum — but a body delimited only by the connection closing
  cannot be told from a complete one by any client, so behind such a hop a mount can land
  short. That is the price of mounting the file at all, against a refusal that was certain.
  Spooling, rather than asking someone for the true size, because there is nobody to ask.
  The session resource carries no size — it mirrors the reference's file resource field for
  field, which has none — and the environment key a worker holds admits exactly one file
  route, `GET /v1/files/{id}/content`; the metadata read beside it is management-key only,
  deliberately, and widening that lane to save a temp file would be the wrong trade. (The
  issue as filed guessed at the one-line version of this fix, that the worker could look the
  size up. It cannot.)

- **A streaming write refuses a size that is not a length before it does anything**
  (#386). What the sandbox interface promised was only half of what a caller needed to know:
  it required `size` to equal the bytes the source yields, which is a rule about the stream,
  and said nothing about a caller who has no number at all. It now says that too, and the
  shared contract suite requires it of every backend. Both backends already refused a `-1`,
  each by accident and by a different route — one comparing the delivered count against a
  number nothing could equal, the other handing it to an archive writer that complained —
  but each of them only after the work was done: the target's parent directory created in
  the agent's sandbox, and on Kubernetes every byte of the reader carried into the pod and
  written to its disk before the count was looked at, so a 500 MB reader could fill a pod
  that was always going to reject it. One `CheckWriteSize` at the top of both writes settles
  it before the first byte moves, and the contract asks for it by its consequences: the
  refused write's parent must not appear. With the guard removed the row fails on both
  backends with the parent present.
  The in-memory sandboxes the worker, executor, toolset and shell suites run against did not
  refuse it at all: they read `-1` through an `io.LimitReader` as "no bytes", wrote an empty
  file and reported success — which is how a defect this size reached production with the
  suite green, since the run logged `file materialized` and recorded the mount in its
  sentinel, so the next pass would skip re-streaming it. All four now call the same
  `CheckWriteSize` the backends do. Measured both ways: against the pre-fix worker the new
  end-to-end row fails with the mount absent and the sentinel empty, and with the old fake
  restored it fails with the mount present, empty, and logged as materialized.
