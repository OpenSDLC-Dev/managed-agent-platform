---
status: in-progress
issue: "#1052"
---

# Vault destination binding and egress gate hardening (plan 70)

#1052 tracks five security hardening items from a static review of `main` at `822c775`. This plan covers the three Vault and egress-gate changes. Login throttling and issue-triage tooling land in separate focused pull requests.

## Scope

1. **Slice 1 — Vault credential destination binding.** Prevent an `environment_variable` credential update from expanding where an existing write-only secret is sent without supplying new secret material. Bind the sealed secret to its approved destination policy so tampered rows fail closed at gate resolution too.
2. **Slice 2 — Egress gate resource limits.** Bound concurrent in-flight requests, active tunnels, and aggregate plain-HTTP body buffering across policy refreshes. Apply independent container resource limits to the gate sidecar on both Docker and Kubernetes.
3. **Slice 3 — Egress policy withdrawal.** Apply the current policy and credential set at outbound admission rather than keeping stale handler snapshots. Define and enforce the withdrawal contract for active requests and CONNECT tunnels when destinations, credentials, or session tokens are withdrawn.

## Decisions

### Slice 1: Vault credential destination binding

1. **Require `secret_value` on destination expansion for all roles.** When `POST /v1/vaults/{id}/credentials/{cid}` updates an `environment_variable` credential and omits `secret_value`, the new `networking` must be covered by the stored `networking`.
   - `unrestricted` covers every valid `networking` policy.
   - `limited` never covers `unrestricted`.
   - `limited(old)` covers `limited(new)` only when `egress.HostSet(old).CoversEntry(h)` holds for every entry `h` in `new`.
   - Any expansion without `secret_value` returns `400 invalid_request_error`. Preserving destinations, narrowing destinations, omitting `networking`, updating `injection_location`, and metadata-only or `display_name`-only updates remain allowed without `secret_value`.
   - This mirrors the existing `token_endpoint_auth` arm-switch rule in `internal/api/vaultcredauth.go` and is recorded in `docs/DIVERGENCES.md` as a deliberate local hardening divergence.
2. **Seal a fixed-size destination digest inside the encrypted secret map.** On create and on every auth update, store `approved_networking_sha256` inside the cipher-authenticated secret JSON map (`egress.NetworkingBindingDigest` over canonicalized, deduplicated, sorted entries).
   - Fixed 64-hex-byte overhead avoids pushing large `allowed_hosts` lists past Cloud KMS HSM plaintext limits.
   - Narrowing re-seals the narrower digest, so a caller cannot narrow and later re-expand without supplying `secret_value`.
   - `internal/vaultresolve.Credentials` verifies the digest against `c.auth.networking` whenever `approved_networking_sha256` is present and fails closed on mismatch. Existing credentials written before this change lack the key and remain readable until their next auth update.
   - `updateVaultCredential` compares both `secret_ciphertext` and `auth` in its `FOR UPDATE` compare-and-set check.

### Slice 2: Egress gate resource limits

1. **Share a limiter across policy swaps.** Concurrency limits (plain-HTTP requests and CONNECT tunnels) and the aggregate plain-HTTP request-body buffer budget live on a shared `gate.Limiter` (or long-lived gate state) rather than per swapped policy instance, so a 30-second config refresh never doubles or resets in-flight budgets.
2. **Account buffered body bytes incrementally and release on transport close.** Reject declared `ContentLength > MaxBodyBytes` with `413` before reading. Acquire from the aggregate buffer budget as request body chunks are read and incrementally as substitution expands, reject with `503 Service Unavailable` when concurrency or buffer budgets are exhausted, bound stalled outbound uploads separately from response streaming, and drop the backing buffer and release its reservation when the outbound body closes.
3. **Apply independent container limits on both sandbox backends.** The gate sidecar runs outside the sandbox's cgroup/container limits, so Docker (`Memory`, `NanoCPUs`, `PidsLimit`) and Kubernetes (`Resources.Requests` and `Resources.Limits` for CPU and memory, plus opt-in ephemeral storage so a default gate disk cap does not impose a pod-wide storage eviction limit on an unbounded sandbox) apply explicit gate defaults regardless of whether sandbox memory limits are enabled.

### Slice 3: Egress policy withdrawal

1. **Live policy updates on one long-lived `Gate`.** Replace swapping isolated `*gate.Gate` instances with atomic policy-and-credential snapshot updates (`Gate.Update`) on one `Gate` that owns the dialer, transport, limiter, and active-work registry. `SwappableHandler` delegates to it (or updates it in place) and calls `CloseIdleConnections()` on every policy change so withdrawn hosts cannot reuse pooled idle sockets.
2. **Enforce current policy at outbound admission.**
   - Plain HTTP: admit on entry for fast fail, buffer the raw body without substituting, then immediately before `RoundTrip` load the current snapshot, re-check host admission, and substitute using the current credential engine. Inside the dialer, re-verify the current snapshot right before opening the TCP connection so a swap during DNS/queue wait cannot open a newly disallowed socket.
   - CONNECT: admit on entry, re-verify in the dialer before TCP connect, and register the live tunnel (host, port, admission class, resolved remote IP, connections) while it runs.
3. **Withdrawal contract for active work.**
   - When a policy update (including 401 deny-all revocation) withdraws a destination host or tightens its admission class so the tunnel's resolved IP violates the address floor, the gate immediately closes matching active CONNECT tunnels.
   - Active plain-HTTP requests whose destination host is no longer admitted, or that substituted a credential whose secret or destination permission for that host was withdrawn or changed, have their upstream context cancelled immediately.
   - Active tunnels and requests whose destination and used credentials remain admitted continue uninterrupted.

## Pull requests

1. **PR 1:** This plan, `STATE.md`, Slice 1 (Vault destination binding), `docs/DIVERGENCES.md`, `docs/ARCHITECTURE.md`, and changelog fragment.
2. **PR 2:** Slice 2 (egress gate concurrency/buffer limits and Docker/Kubernetes sidecar resource limits), `STATE.md`, docs, and changelog fragment.
3. **PR 3:** Slice 3 (egress policy withdrawal and active tunnel/request teardown), plan archival (`status: archived`), `STATE.md`, docs, and changelog fragment.

## Verification

- Slice 1: `internal/egress`, `internal/api`, and `internal/vaultresolve` tests covering expansion refusal, narrowing/equivalence/metadata updates, legacy credentials, and tampered DB rows.
- Slice 2: `internal/gate`, `internal/gaterun`, `internal/sandbox/docker`, and `internal/sandbox/k8s` tests covering concurrent buffer overload, ordinary request completion under load, and explicit container resource limits on both backends.
- Slice 3: `internal/gate` and `internal/gaterun` tests covering superseded-policy plain-HTTP refusal before outbound dial, credential withdrawal before substitution, active CONNECT tunnel teardown on host withdrawal and revocation, and uninterrupted unaffected tunnels.
- Every PR: `make verify`, verifier review, and dual code review.
