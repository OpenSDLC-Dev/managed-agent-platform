# Plan 69 authentication upgrade

Plan 69 is unreleased. Its final rollout has two deployments: native-only code
(slice 7a), then obsolete-data cleanup (slice 7b). Do not skip the first deployment.
The active rollout and acceptance records are in [STATE.md](../STATE.md) and
[HISTORY.md](./HISTORY.md). This note describes the operator contract, not completion.

## Before deploying native-only code

1. Deploy slice 6 and verify native Console login through the canonical HTTPS origin.
   Production uses OIDC or a signed trusted proxy. Password login requires declared
   development mode. The control plane and gateway require a login mode and origin.
2. Sign in as a human admin. Issue owned keys for each API consumer. For CD, create
   a developer service account, issue its key and store it in Secret Manager.
   Set `CD_API_KEY_SECRET_NAME` to that secret's name. Keep raw values out of logs.
3. Replace every unowned key, including old Console-issued keys. Neither its issuer
   nor its name makes it owned. Confirm both `x-api-key` and issued-key Bearer access
   to public APIs. Private routes and gateway administration require browser sessions.
4. Review scheduled deployments and ongoing sessions or dreams. Historical unowned
   initiators cannot authorize another model turn or scheduled run after this upgrade.
   A scheduled deployment fails with `unknown_error` and auto-pauses. An admin can
   select an active service account for future runs, then unpause it. A session's
   initiator is immutable; start replacement work under an active member if needed.
5. Review `IDENTITY_ROLE_MAP`: legacy `viewer` means `user`, which cannot read resources.
   Use `developer` for users who need resource access. Mapping runs at sign-in only.
6. Back up database and deployment configuration through the installation's normal
   backup process. Record the slice 6 Helm revision and Console image, Secret inventory,
   ingress backend, certificate and any backend-scoped IAP bindings.

## Deploy and accept slice 7a

Deploy the native-only Console image with the matching platform chart or Compose file.
Console holds no credentials. Remove `CONTROLPLANE_API_KEY`, `BRAIN_API_KEY` and
`PLATFORM_API_KEY` from runtime configuration. Remove Helm `controlplane.apiKey`,
`brain.apiKey`, `console.authMode` and `console.legacySecret` values, and Compose's
`CONSOLE_AUTH_MODE` switch. They no longer select authentication or routing.
The brain's default gateway route uses `delegated_inference: true` and per-claim tokens.

Drain old replicas before relying on the new authorization boundary. Mixed versions
can refuse requests during rollout. Public keys use their owner's current role;
gateway inference requires developer or admin. An active member's existing delegated
work survives role changes but stops after membership removal.

The gateway no longer reads grants, key policies or per-key rate windows. Organization
RPM, input TPM and output TPM limits share each public model alias across callers,
protocols and backends. Wildcard matches share one `*` budget. No per-key limit remains.

Before cleanup, verify native login, private cookie access, an owned key in both public
headers, refusal of old credentials, model calls and delegated session work. CD checks
HTTPS and its issued key; it does not replace a complete session acceptance run.

Slice 7a retains obsolete database tables for rollback. The GCP assembler also preserves
existing data in its externally managed Kubernetes Secret and fetches no retired preset value.
For chart-managed Secrets, Helm rollback restores the previous revision's Secret manifest.
The GCP assembler preserves existing data while
updating current fields, with a resource-version check against concurrent rotation.
Retain the old Secret Manager versions until slice 7a is accepted.

If acceptance fails, restore the recorded slice 6 chart revision and its Console image
together. Keep the native HTTPS origin and login configuration. Restore saved ingress
and IAP bindings if those changed. Verify login and SDK access after rollback.
Do not copy owned hashes into historical columns, clear owners or undo tombstones.

## Cleanup after acceptance

Only after slice 7a passes, deploy migration `0066_retire_gateway_key_policies.sql`.
It drops `modelgateway.key_policies` (including alias grants), its guard function,
and `modelgateway.rate_windows`. It keeps historical key rows, usage, initiator
attribution and organization model budgets. The staging chart changes the control-plane
Service to `ClusterIP`; browser, SDK and CD traffic use the HTTPS application edge.

Remove `controlplane-api-key` and `brain-api-key` from the externally managed
Kubernetes Secret. Remove its `kubectl.kubernetes.io/last-applied-configuration`
annotation too: that annotation can retain the old values. Guard the update with the
observed resource version so a concurrent rotation cannot be overwritten. Preserve
every other field. Disable the corresponding old Secret Manager versions; do not
destroy them during acceptance. Later CD runs must not restore the removed fields.

Confirm the active ingress points to the native application edge. Delete the old
standalone Console Deployment and wait for its pods to terminate. Then remove its
unused Service, BackendConfig, NetworkPolicy and credential Secret.
Preserve the shared HTTPS origin, address, certificate, active application ingress
and native BackendConfig. Confirm the old HTTP address no longer has a listener.

After this cleanup, the rollback target is the accepted slice 7a application build.
Keep the cleanup chart and values when selecting those application images, so rollback
does not recreate the HTTP listener or restore retired credentials. Earlier binaries
query tables that no longer exist. Returning to them requires coordinated database and
configuration restoration; a Helm-only rollback is insufficient.
