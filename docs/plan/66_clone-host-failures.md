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

The first two never reach the error at all: go-git takes an empty or short
pack response for "already up to date" (`transport.ErrEmptyUploadPackRequest`,
`remote.go:577`), and the checkout then misses the commit.

## Decisions

1. **The host's in-band messages read `network`** (the owner, 2026-10-15): an
   `ERR` line, a sideband error-band message and a `NAK` with no pack, all of
   them, with no split by their text. It follows #939's rule that a host
   which does not speak git reads `network`, and it keeps no table of host
   wording. A refusal of the host's response by go-git's decoder reads
   `network` by the same rule. They are matched by go-git's types and
   sentinels, and by go-git's own words where it kept only text
   (`error decoding upload-pack response: `, the sideband's
   `unexpected error: `), never by the host's.
2. **A transport records each clone's failures.** go-git's HTTP transport is
   installed once per process (`client.InstallProtocol`, for `http` and
   `https`): `http.DefaultTransport`, as go-git's own client uses, wrapped by a
   `RoundTripper` that notes each failure into a record the clone puts in its
   context. go-git passes the clone's context to every request, so the record
   is the clone's alone, and a request with no record passes through as
   before. The record notes the first failed request, the first failed read of
   a response body, and a pack response that ended before its first byte. It
   notes nothing while the clone's context is done: those errors are the
   deadline's or the cancellation's.
3. **A recorded failure decides `network` only over `internal` and
   `checkout`.** Those are what go-git concluded from what was missing. `auth`,
   `not_found`, `too_large` and `timeout` came from what did arrive or from this
   machine, and keep their reasons. So does an error this executor made
   (`isLocalError`).
4. **The context's error joins wherever its text was all go-git kept.** A
   decode refusal now reads `network`, and go-git formats a deadline or a
   cancellation inside the decode with the same words. So the clone's context
   error joins the chain when the context has ended, the message names it,
   and the reason would otherwise be `internal` or `network`. Before, only a
   deadline joined, and only over `internal`.
5. **A commit the host does not have still reads `checkout`.** The host
   refuses it with an `ERR` line, and `fetchCommit` already marks that refusal
   as the caller's sha before `cloneReason` reads it.

## Verification

- The table above, one fixture row each, through `cloneToTar`: every row reads
  `network`; an unknown commit still reads `checkout`.
- The record: per clone, silent without one, silent once the context is done,
  and only over `internal` and `checkout`.
- The deadline and cancellation rows #939 pinned keep their reasons.
