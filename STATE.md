# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) in [CHANGELOG.md](./CHANGELOG.md), the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

[#83](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/83) — provider adapters quoted a
failing endpoint's response body verbatim, so an endpoint echoing the request's auth header back put
the model credential into an error that lands in an append-only `session.error` event. No plan file
(single-PR fix; triage returned `needs_plan: false`).

## Tasks

- [x] Every leak path reproduced by a test that fails on *finding* the secret. The issue named two;
      three more were openai's mid-stream error frame, anthropic's post-`Next()` `Err()` (both under
      HTTP 200, where `Generate` returns nil), and a `base_url` credential the SDK prints itself.
- [x] `provider.NewRedactor` matches configured secrets by exact value, not by token shape — the
      observed echo was a bare token no `Bearer`-shaped matcher would have caught. Non-auth header
      values are left alone so a routing tag survives in the diagnostic.
- [x] `Redactor.Error` overrides `Error()` and keeps `Unwrap`: `%w` would re-render the leaking
      message, but discarding the chain would block retry logic reading an upstream status.
- [x] `make verify` green; two verifier passes and two reviewers, all findings addressed.
- [x] Review rounds found a credential is not one string but every encoding the stack renders it in:
      percent-encoded, base64 in a derived `Authorization: Basic`, as written. The original fixture
      was URL-safe — the one class that could not see the gap. Also fixed: custom auth header names,
      username-only userinfo, `resp.Status`, and an over-redaction that ate a routing tag.
- [x] Docs corrected: `docs/ARCHITECTURE.md`'s security invariant claimed this redaction already
      existed (false when written); two residuals recorded as deliberate.
- [ ] PR open, CI green, review threads settled.
