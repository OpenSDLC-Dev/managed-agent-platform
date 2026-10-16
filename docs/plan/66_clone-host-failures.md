---
status: archived
issue: "#940"
---

# The git host's failures that go-git hides (plan 66)

A failed repository clone's `session.error` names whose fault it was (#939).
`cloneReason` reads go-git's error, and go-git keeps too little of some of the
host's failures for that. A fixture behind `git http-backend` measured these
host failures with go-git v5.19.2; the last two rows came in review:

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
   and sentinels, and by go-git's own words where it kept only text: the
   pkt-line scanner's refusal, `error decoding upload-pack response: `, the
   sideband's `unexpected error: `, `unknown channel ` and
   `max. packed size exceeded`, and `malformed pack file signature`. Never
   by the host's. A pack that is corrupt past its signature is out of
   this plan's scope and keeps its reason.
2. **The clone's transport marks the host's failures where go-git reads
   them.** go-git's HTTP transport, installed for the process as the executor
   package initializes, answers a clone's failed body read with a marked
   error of its own, which go-git returns with the clone's error. go-git
   compares only a bare `io.EOF` and `io.ErrUnexpectedEOF` for "already up
   to date" (`upload_pack.go:64`), so the marks pass through it. A transport
   that wraps the connection instead would keep go-git's
   `*http.Transport` type, but under TLS it cannot see a response's body, so
   it could not tell an empty pack response. The cost: go-git refuses an
   endpoint that names its own CA bundle, client certificate, proxy or
   `InsecureSkipTLS`, and no clone names one.
3. **The marks decide by their words.** go-git formats a failed read with
   `%s` inside its decode, so `cloneReason` reads a mark by its words. The
   first mark in a message is the transport's: what follows it is the error
   it wraps, which can quote the host's words. A record kept beside the clone
   and matched to go-git's error afterwards was this plan's first design:
   every review round found another way the two failed to match, and the
   marks replaced it. A random token in each mark's words would make them
   unforgeable, but it would sit in every logged clone error, and the only
   forgery left blames this platform for the host's own failure.
4. **The context's error joins wherever its text was all go-git kept.** A
   decode refusal now reads `network`, and go-git formats a deadline or a
   cancellation inside the decode with the same words. So the context's
   error now joins over `network` as well as `internal`; before, only a
   deadline joined, and only over `internal`. Where go-git keeps the host's
   words, or the transport's mark, in a type, they stay the host's.
5. **A commit the host does not have still reads `checkout`.** The host
   refuses it with an `ERR` line, and `fetchCommit` already marks that refusal
   as the caller's sha before `cloneReason` reads it.

## Verification

- The table above, one fixture row each, through `cloneToTar`: every row reads
  `network`; an unknown commit still reads `checkout`.
- The marks: on a marked clone's success bodies only, none on a read the
  context's end failed, an empty pack response whatever the context, this
  machine's apart from the host's, and the first mark in a message read as
  the transport's.
- The deadline and cancellation rows #939 pinned keep their reasons.
