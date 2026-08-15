- **`internal/identity` — the human-authentication boundary** (#56). A strict OIDC
  relying party for any compliant provider, plus a trusted-proxy mode (`gcp-iap` preset
  or `custom`) for deployments where the cloud terminates authentication. `Verify`
  authenticates one compact JWT — the signature allowlist is
  RS256/RS512/ES256/ES384/ES512, so `alg:none` and HS256 never reach a key lookup —
  returning a principal whose role (`viewer` < `developer` < `admin`) comes from a
  configurable claim. How that claim name is read is fixed at configuration time so a token
  cannot choose it: a URI-shaped name (`https://corp.example/roles`, the Auth0 convention) is
  one flat key, dots included, while any other dotted name (`resource_access.console.roles`,
  the Keycloak shape) is a path. Signing keys are cached for five minutes, so a revoked key
  stops verifying within that bound. `IDENTITY_MODE` is `disabled` by default; `oidc` and
  `trusted_proxy` read the `IDENTITY_OIDC_*` / `IDENTITY_PROXY_*` / `IDENTITY_CLAIM_*`
  variables and a required `IDENTITY_ROLE_MAP`, and any misconfiguration fails startup
  rather than open. No route consumes it yet; the `/v1` wire, the `ant` CLI and machine
  credentials are untouched.
