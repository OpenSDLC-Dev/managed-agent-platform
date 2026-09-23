---
status: archived
issue: 738
---

# The upload filename rule, and the registry claims the SDK read contradicted

## Contract and scope

Plan 51's slice 2 read every SDK citation in docs/DIVERGENCES.md at its tag and
left six claims standing that the read contradicted, because each changed an
argument rather than its evidence (#738). Five are registry text. The sixth is a
wire behaviour: the API reference now documents what `POST /v1/files` does with
the `file` part's filename — "Only the final path component of the part's
`filename` is kept; an absent or empty `filename` is replaced with `unnamed` plus
the extension for the file's stored `mime_type`, when known" (checked against
anthropic-sdk-go v1.70.1 — betafile.go BetaFileUploadParams.File) — and this
platform answered 400 to both. Public docs outrank an inference, and the Go SDK
reaches the empty case whenever `File` gets no name over a reader without one, so
the handler follows the rule.

1. **Order**: cut the part filename to its final component, derive the MIME type
   (the part's Content-Type when specific, else that name's extension, else
   `application/octet-stream`), name an empty result `unnamed` plus the type's
   extension, then validate. Deriving the type first is what lets `dir/report.pdf`
   sent as octet-stream store `report.pdf` as `application/pdf`.
2. **Separators**: the rule does not say which characters separate components.
   `/` and `\` both do, because a `\` comes from the Go SDK itself: on Windows it
   names an open file by `path.Base`, which cuts only at `/`, and sends the whole
   `C:\...\report.pdf`. Refusing that would block a real client, where cutting a
   `\` the reference might refuse only accepts more. `dir/` and `dir\` leave an
   empty name, and the guide's other forbidden characters apply to what remains.
3. **Extensions**: `mimetab.ExtFor` inverts the pinned table, so the name maps
   back to the stored type's bare media type (every listed type round-trips
   through `ByPath`, with the table's parameters). A type it lists under several
   extensions takes its conventional one (`.jpg`, `.txt`, `.html` …), and
   `application/octet-stream` takes `.bin`, as Python's `mimetypes` does; an
   unlisted type leaves a bare `unnamed`. No content sniffing: the guide says an
   omitted type "is detected", and nothing says how, so a nameless part without
   a type is `unnamed.bin`.
4. **Content-Type under the old beta header**: the guide calls the part
   Content-Type "Required" with `files-api-2025-04-14`. This platform ignores beta
   headers, so a part without one is taken either way; recorded, not enforced.
5. **Registry corrections**, evidence and argument alike: the work-item residual
   now weighs the SDK worker's own statement that the queue sends `healthcheck`
   items; the `initial_events` caps are cited to the sessions page, which states
   all three, where the SDK states one; the agent-archive entry counts the
   deprecated Admin API twin of the tunnel archive; a misquotation loses its
   "that"; and the memory-marker entry drops a `find -path` the reference never
   had, and its collision premise — the reference's worker skips a listed memory
   at the marker path rather than overwrite anything.

What stays inferred — the extension a nameless upload takes, whether `\`
separates, and the rejection surface — is in the DIVERGENCES entry, under #78.

## Verification

- Tests first, red on the old handler: every rule case over a verbatim
  Content-Disposition (absent, empty, charset, no type, unlisted type,
  path-qualified, trailing slash, Windows path, trailing backslash, a forbidden
  character after the cut), the stored name read back, and two uploads through
  the pinned SDK — `File` with no name, and a reader named by a Windows path.
- `ExtFor` against the table: round trip, pinned ambiguities, parameters and case.
- The registry edits pass `tools/sdkref` (`-fail` and `-report`) and
  `tools/registrycheck`; `make verify`, independent verification, both reviews
  and the PR's CI before squash merge.
