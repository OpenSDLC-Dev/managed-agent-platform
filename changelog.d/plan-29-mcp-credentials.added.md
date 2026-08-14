**MCP servers authenticate with the session's vault credentials.** A session whose attached
vaults hold an `mcp_oauth` or `static_bearer` credential for a server its agent declares now
dials that server with the token as `Authorization: Bearer`, on both paths that dial —
discovery and the tool call. Matching is by URL after the reference's normalization, first
vault with a match wins, and a server no credential matches is dialled unauthenticated.
Credentials are resolved on every dial, so a rotation or an archive reaches the next one
without a restart.

**A refused credential is now reported as one.** A 401 or 403 from the server — its refusal,
or its requiring a credential where none matched — is `mcp_authentication_failed_error`
rather than `mcp_connection_failed_error`, as is a credential this platform could not
open, whose dial is not made at all. A lookup that fails for its own reasons is neither: it
retries. Every other failure is unchanged.

OAuth refresh of an expired `mcp_oauth` token is the next slice.
