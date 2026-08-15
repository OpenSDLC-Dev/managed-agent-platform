- **A BYOC worker mounts its session's files even when the download declares no
  length** (#386). The worker passed the download's `Content-Length` to the sandbox
  write seam unchecked; Go reports `-1` when an intermediary chunks or decompresses the
  body, so the seam refused it and every such mount was skipped, leaving the session an
  empty workspace the system prompt still described as mounted. A length-less body is
  now spooled to a temp file to measure it, bounded at the Files API's 500 MB per-file
  cap and refused rather than truncated above it; the spool is unlinked immediately, so
  a killed worker leaves no customer bytes on disk. Streaming writes now reject a
  non-length size before the first byte moves — a sandbox-contract rule every backend
  must pass. One limit remains: a body delimited only by the connection closing cannot be
  told from a complete one by any client, so behind such a hop a mount can still land short.
