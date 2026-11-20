---
status: in-progress
issue: "#995"
---

# Console Playground (plan 73)

The Playground sends Messages requests to the model gateway through the
Console origin. The [browser recording](https://github.com/OpenSDLC-Dev/managed-agents-wire-recordings/tree/main/2026-10-22-playground-protocol)
supplies the cookie, path, request body and streaming contract. The gateway
supplies its chat model aliases through the private Models collection.

## Decisions

- An unmatched IdP sign-in still receives `user`. An admin must explicitly
  enable Playground access for that member. An IdP mapping that grants
  `user` alone does not grant spending authority. Developer and admin keep
  their existing authority to call models.
- The Console uses `GET /api/organizations/default/proxy/v1/models` and
  `POST /api/organizations/default/proxy/v1/messages?beta=true`. The edge
  forwards those exact paths to the gateway. The private gateway lane uses
  the native cookie, checks CSRF on POST and attributes usage to the current
  principal. It uses the shared model limits and keeps browser credentials
  out of upstream requests. Open streams recheck current session authority.
- The admin Members screen grants and withdraws access independently of the
  IdP role. A `user` lands on Playground and sees an access message when
  approval is pending. Management routes keep their existing role floor.
- The Playground supports selecting a model, entering a prompt and optional
  system instruction, and running or stopping a streamed response. The
  browser does not persist drafts or generate provider credentials. The
  Console checks the session capability and the edge's gateway-disabled signal.

## Acceptance

Use the same-origin edge, Console, gateway and PostgreSQL with a fixture
model. Verify first sign-in, denial, admin grant, model discovery, Messages
streaming, Stop, withdrawal and principal attribution. Verify cookie and
role boundaries on management, raw inference and private gateway routes.
