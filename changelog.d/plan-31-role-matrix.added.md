- **The role matrix — every route says what it needs** (plan 31 slice 3, #56) —
  slice 2 gated the control plane at a floor that denied every human; each
  identity-reachable route now declares its minimum role instead. Reads are
  `viewer`, the streaming ones included; resource CRUD and session lifecycle are
  `developer`; vault-credential create, update, delete, archive and
  `mcp_oauth_validate` are `admin`, alongside the whole environment-key surface,
  listing included. Vault-credential *reads* stay `viewer` — they return sealed
  metadata, never a secret. Two edges are deliberate and look like mistakes until you know
  why: the vault itself is `developer`, so deleting or archiving one purges the credentials
  inside it — `admin` bounds who may read, write or mint a secret, not who may destroy the
  container holding it — and the session-resource routes stay `developer` although they take
  a `github_repository` `authorization_token`. The work API and the gate-config route keep
  `RoleNone`, which no role satisfies, so a human gets 403 there; worker
  environment keys and the management `x-api-key` are unaffected. A denial names
  the route's requirement, not the caller's role, and no path or method moved.
