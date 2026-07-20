# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#83](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/83) — provider adapters quoted a
failing endpoint's response body verbatim, so an endpoint echoing the request's auth header back put
the model credential into an error that lands in an append-only `session.error` event. No plan file
(single-PR fix; triage returned `needs_plan: false`).

## Tasks

- [x] All five leak sites reproduced by test, each failing on *finding* the secret: the two the issue
      named, plus openai's mid-stream error frame, anthropic's post-`Next()` `Err()` (both under
      HTTP 200), and a `base_url` userinfo password the SDK prints with `String()`, not `Redacted()`.
- [x] `provider.NewRedactor` matches configured secrets by exact value — api key, `base_url`
      password, auth-named header values — since the observed echo was a bare token that a
      `Bearer`-shaped matcher would have missed (`internal/provider/redact.go`).
- [x] Non-auth header values deliberately not redacted, so a routing tag still reads back out of the
      diagnostic the quoted body exists to provide.
- [x] `Redactor.Error` overrides `Error()` and keeps `Unwrap`: `%w` would re-render the leaking
      message, but discarding the chain would block retry logic reading an upstream status.
- [x] Docs corrected: `docs/ARCHITECTURE.md`'s security invariant claimed this redaction already
      existed (false when written); both integration-test comments and `evals/report_test.go`'s
      "cannot reach an error" premise updated.
- [x] `make verify` green; two verifier passes, both PASS with findings, all addressed.
- [x] Review round: a `base_url` password is stored decoded but printed re-encoded, so it leaked in
      full unless URL-safe — the original test fixture was, which is why it passed. Re-verification
      then found a fourth rendering: `net/http` derives `Authorization: Basic` from userinfo, so an
      auth-echoing endpoint quotes the credential base64-encoded. Every rendering is now registered,
      the quoted body over-read so truncation cannot sever a secret. Each gap fails without its fix.
- [x] External reviewer round: custom auth header names, a username-only userinfo, and an
      endpoint-controlled `resp.Status` all leaked; splitting a header value on any space
      over-redacted a routing tag. Two residuals documented as deliberate (a key re-encoded by
      Go's HTML-escaping JSON encoder; a model's own successful output).
- [ ] PR open, CI green, review threads settled.
