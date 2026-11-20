---
status: archived
issue: "#993"
---

# Interactive CLI OAuth (plan 72)

The CLI opens the platform's Console origin for authorization. It exchanges
the code at the control plane and can use the resulting token on the control
plane and the inference gateway. [The reference recording](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/tree/main/2026-10-31-cli-oauth-scopes)
supplies the token and refresh shapes and the scope admission matrix.

## Decisions

- Admit the registered public CLI client. Accept its localhost callback with
  an ephemeral port and its exact manual callback on the configured Console
  origin. Require S256 and bind the code to the client, callback and state.
  See [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252.html).
- Authorize only after a user with a current native browser session consents
  by same-origin POST. The authorization page links to the existing sign-in
  page when the user has no session. The OAuth request stays in the first tab.
- Store credential hashes. A code is single-use. Access lasts eight hours.
  A refresh grant lasts at most 30 days. Rotate refresh tokens and retire the
  grant when a spent token is replayed, following
  [RFC 9700](https://www.rfc-editor.org/rfc/rfc9700.html#section-4.14.2).
  Principal removal revokes the grant permanently.
- The scope check and the current stored role apply to every resource
  request. The three user scopes and `org:admin` have the recorded limits.
  The admin Workspace listing returns the empty collection for this
  single-workspace install. Private browser and worker routes keep their
  existing credential rules.
- Route authorization to the control plane at the Console origin. The token
  endpoint stays with public management and inference keeps its own gateway
  origin. Both services read grant authority from the same database.

## Acceptance

Drive browser approval, code exchange, refresh, and a management request
with the published CLI. Exercise each recorded scope set against the
control plane and gateway. Check PKCE, state, redirect validation, code use,
refresh replay, role withdrawal and private-lane refusal through HTTP.
