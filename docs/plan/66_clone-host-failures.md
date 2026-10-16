---
status: in-progress
issue: "#940"
---

# The git host's failures that go-git hides (plan 66)

A failed repository clone's `session.error` names whose fault it was (#939).
`cloneReason` reads go-git's error, and go-git keeps too little of some of the
host's failures for that. A fixture behind `git http-backend` measured these
host failures, with go-git v5.19.2, before this plan:

| The host | go-git's error | Reason before |
|---|---|---|
| answers the pack request with an empty 200 | `invalid reset option: object not found` (a commit: `object not found`) | `checkout` |
| closes the connection after the pack response's headers | the same | `checkout` |
| breaks the pack response's chunked framing | `invalid byte in chunk length` | `internal` |
| answers `NAK` and no pack | `git.ErrFetching` (a commit: `packfile.ErrEmptyPackfile`) | `internal` |
| sends a message on the sideband error band | `unexpected error: <message>` | `internal` |
| sends an `ERR` line in the pack response | `error decoding upload-pack response: <message>` | `internal` |
| sends a pack response go-git's decoder refuses | `error decoding upload-pack response: unexpected content …` | `internal` |
| sends an `ERR` line in the ref advertisement | `*pktline.ErrorLine` | `internal` |
| sends a ref advertisement that is pkt-line but not git's | `*packp.ErrUnexpectedData` | `internal` |
| sends a sideband packet on a band git has no use for | `unknown channel <packet>` | `internal` |
| sends a pack band that carries no pack | `packfile.ErrBadSignature` | `internal` |

The first two never reach the error at all: go-git takes an empty or short
pack response for "already up to date" (`transport.ErrEmptyUploadPackRequest`,
`remote.go:577`), and the checkout then misses the commit.

## Decisions

1. **The host's in-band messages read `network`** (the owner, 2026-10-15): an
   `ERR` line, a sideband error-band message and a `NAK` with no pack, all of
   them, with no split by their text. It follows #939's rule that a host
   which does not speak git reads `network`, and it keeps no table of host
   wording. A refusal of the host's response by go-git's pkt-line, pack
   response and sideband decoders reads `network` by the same rule, and so
   does a pack band that carries no pack. They are matched by go-git's types
   and sentinels, and by go-git's own words where it kept only text
   (`error decoding upload-pack response: `, the sideband's
   `unexpected error: `, `unknown channel ` and `max. packed size exceeded`),
   never by the host's. A pack that is corrupt past its signature is out of
   this plan's scope and keeps its reason.
2. **A transport records each clone's failures.** go-git's HTTP transport is
   installed as the executor package initializes (`client.InstallProtocol`,
   for `http` and `https`), because go-git's protocol map has no lock:
   `http.DefaultTransport`, as go-git's own client uses, wrapped by a
   `RoundTripper` that notes each failure into a record the clone puts in its
   context. go-git passes the clone's context to every request, so the record
   is the clone's alone, and a request with no record passes through as
   before. The record notes the first failed request, the first failed read of
   a success's body, and a pack response that ended before its first byte. It
   reads no other body: the HTTP client drains a redirect's body and ignores
   a failure there, and go-git reads a refusal by its status. It notes nothing
   while the clone's context is done: those errors are the deadline's or the
   cancellation's. Every go-git clone in a binary that imports the executor
   goes through it, the evals suite's among them. go-git refuses an endpoint
   that names its own proxy, CA bundle, client certificate or
   `InsecureSkipTLS` with this transport, which is not an `*http.Transport`.
   None of those clones names one; the system's trust store and the proxy
   environment variables apply as before. A transport that wraps the
   connection instead would keep go-git's type, but under TLS it cannot see
   a response's body, so it could not tell an empty pack response.
3. **A recorded failure decides `network` only over `internal` and an object
   the checkout missed.** Those are what go-git concluded from what was
   missing. `auth`, `not_found`, `too_large` and `timeout` came from what did
   arrive or from this machine, and keep their reasons. So does the caller's
   ref, as the answer that did arrive shows it: a branch the ref
   advertisement lacks, a commit the host says it does not have, an id that
   names a tree or a blob. So does an error this executor made
   (`isLocalError`). The mark goes on only where it decides the reason, so a
   refused proxy, whose error go-git keeps, is not named twice in the log. A recorded failure that is this executor's own joins the
   chain as it is, because go-git formats it with `%s` inside the decode.
4. **The context's error joins wherever its text was all go-git kept.** A
   decode refusal now reads `network`, and go-git formats a deadline or a
   cancellation inside the decode with the same words. So the clone's context
   error joins the chain when the context has ended, the message names it,
   and the reason would otherwise be `internal` or `network`. Before, only a
   deadline joined, and only over `internal`. It does not join where go-git
   keeps the host's words in a type, an `ERR` line or a response that is not
   git's: those words are the host's, though they quote the context's.
5. **A commit the host does not have still reads `checkout`.** The host
   refuses it with an `ERR` line, and `fetchCommit` marks that refusal as the
   caller's sha (`errCommitRefused`) before `cloneReason` reads it.

## Verification

- The table above, one fixture row each, through `cloneToTar`: every row reads
  `network`; an unknown commit still reads `checkout`.
- The record: per clone, silent without one, silent once the context is done,
  silent for a redirect's and a refusal's body, and only over `internal` and
  an object the checkout missed.
- The deadline and cancellation rows #939 pinned keep their reasons.
