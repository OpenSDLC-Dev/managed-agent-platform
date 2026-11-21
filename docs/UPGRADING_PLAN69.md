# Plan 69 authentication upgrade

To upgrade an installation from v0.5.1, use the intermediate revisions below.
Back up the database and deployment configuration before deploying slice 6.
The [staging acceptance record](./HISTORY.md#plan-69-completion-and-staging-cleanup)
documents the rollout and links to the rollback checks.

| Step | Platform revision |
| --- | --- |
| Prepare native login and issue owned keys (slice 6) | [`e655ec96`](https://github.com/OpenSDLC-Dev/managed-agent-platform/tree/e655ec962e32f697b538a9a1e81dae698b0267ba) |
| Accept native-only authentication (slice 7a) | [`5687be6e`](https://github.com/OpenSDLC-Dev/managed-agent-platform/tree/5687be6e4511f17934eaee8248ee1cdbd8366146) |
| Apply the cleanup (slice 7b) | v0.6.0 |

Build the intermediate versions from source. For Compose, use the Compose file at
each revision and build its local images. For Helm, push each revision's images to
your own registry. Set `VERSION` to the revision and `RELEASE_REGISTRY` to your
registry and namespace. The following example builds slice 6.

```sh
make release-images PUSH=1 VERSION=e655ec96 RELEASE_REGISTRY=registry.example.com/releases
```

Install the chart from the same revision. Set `image.registry`, `image.repository`
and `image.tag` to match the build. If you use the gate, set `executor.gateImage`
to the matching gate image. Keep the Console image pinned by each revision.

## Before deploying native-only code

Use the slice 6 configuration for your deployment.

- **Compose.** Keep the existing project name, volumes and `CONTROLPLANE_API_KEY`.
  Set a distinct temporary `BRAIN_API_KEY` and start with `--profile console`.
  For direct provider calls, point `MODEL_PROVIDERS_FILE` at the existing routes
  and keep that file available in each checkout. See the
  [slice 6 Compose guide](https://github.com/OpenSDLC-Dev/managed-agent-platform/blob/e655ec962e32f697b538a9a1e81dae698b0267ba/deploy/compose/README.md).
- **Helm.** Upgrade the existing release in its namespace with a values file for
  the new chart. Set `console.authMode=platform` and keep the existing control-plane
  key for slice 6. To keep direct provider routes, set `modelgateway.enabled=false`
  and retain `brain.modelProviders` or the external Secret's `model-providers.json`.
  To adopt the gateway, clear `brain.modelProviders` and supply a distinct `brain.apiKey`
  or its `brain-api-key` Secret field. Configure the gateway catalogue before testing
  model calls. See the
  [slice 6 Helm guide](https://github.com/OpenSDLC-Dev/managed-agent-platform/blob/e655ec962e32f697b538a9a1e81dae698b0267ba/deploy/helm/managed-agent-platform/README.md).

1. Deploy slice 6 and verify native Console login at the configured origin. For
   production, use OIDC or a signed trusted proxy over HTTPS. Password login requires
   development mode and permits a literal loopback HTTP origin. Use `/setup` if the
   installation needs its first administrator.
2. Sign in as a human admin. Issue owned keys for each API consumer. For CD, create
   a developer service account, issue its key and store it in Secret Manager.
   Set `CD_API_KEY_SECRET_NAME` to that secret's name. Keep raw values out of logs.
3. Replace every unowned key, including old Console-issued keys. Neither its issuer
   nor its name makes it owned. Confirm both `x-api-key` and issued-key Bearer access
   to public APIs. Use a browser session for private routes and gateway administration.
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
With the gateway enabled, the brain's default route uses `delegated_inference: true`
and per-claim tokens.

Drain old replicas before relying on the new authorization boundary. Mixed versions
can refuse requests during rollout. Public keys use their owner's current role;
gateway inference requires developer or admin. An active member's existing delegated
work survives role changes but stops after membership removal.

The gateway no longer reads grants, key policies or per-key rate windows. Organization
RPM, input TPM and output TPM limits share each public model alias across callers,
protocols and backends. Wildcard matches share one `*` budget. No per-key limit remains.

Before cleanup, verify native login, private cookie access, an owned key in both public
headers, and refusal of old credentials. Run a model-backed session and check the
configured route. If the brain calls the gateway, verify delegated inference for
that session. If clients call the gateway directly, verify an owned-key inference
request. CD checks HTTPS and its issued key. Run the session checks separately.

Slice 7a retains obsolete database tables for rollback. The GCP assembler also preserves
existing data in its externally managed Kubernetes Secret and fetches no retired preset value.
For chart-managed Secrets, Helm rollback restores the previous revision's Secret manifest.
The GCP assembler preserves existing data while
updating current fields, with a resource-version check against concurrent rotation.
Retain the old Secret Manager versions until slice 7a is accepted.

If acceptance fails, restore slice 6. For Helm, restore the recorded chart revision
and its Console image. For Compose, restore the saved slice 6 environment and run
`docker compose --profile console up --build` from `deploy/compose/` in the slice 6
checkout with the same project name. Keep the native origin and login configuration.
Restore saved ingress and IAP bindings if those changed. Verify login and SDK access
after rollback.
Do not copy owned hashes into historical columns, clear owners or undo tombstones.

## Cleanup after acceptance

After slice 7a passes, back up the database and deployment configuration. Deploy
v0.6.0 to apply migration `0066_retire_gateway_key_policies.sql`. For Helm,
update the image overrides to select v0.6.0. Use the chart defaults for the
public platform images and update `executor.gateImage` if you set it.
It drops `modelgateway.key_policies` (including alias grants), its guard function,
and `modelgateway.rate_windows`. It keeps historical key rows, usage, initiator
attribution and organization model budgets. The staging chart changes the control-plane
Service to `ClusterIP`; browser, SDK and CD traffic use the HTTPS application edge.

On a later upgrade, migration 0069 removes the old system `bootstrap` keys
in the default tenant, including rotated records. It identifies the system
keys by their name and missing issuer and owner. Same-name keys with a recorded
issuer or owner survive. The old seeder could adopt an unowned key and clear
its issuer. The migration removes that record as a system preset. Usage and
initiator history keep their recorded IDs.

If your deployment uses an externally managed Kubernetes Secret, remove
`controlplane-api-key`, `brain-api-key` and any
`kubectl.kubernetes.io/last-applied-configuration` annotation that retained their
values. Guard the update with the observed resource version and preserve every other
field. If you use Secret Manager, disable the corresponding old versions and retain
them through acceptance. Update CD to keep the retired fields absent.

If you ran a standalone Console on Kubernetes, first confirm the active ingress
points to the native application edge. Delete the old Console Deployment and wait
for its pods to terminate. Then remove its unused Service, BackendConfig,
NetworkPolicy and credential Secret. Preserve the shared HTTPS origin, address,
certificate, active application ingress and native BackendConfig. If you retired
a legacy HTTP address as staging did, confirm its listener is gone.

After this cleanup, the rollback target is the accepted slice 7a application build.
For Helm, select those application images with the cleanup chart and values. For
Compose, rebuild from the slice 7a checkout with the same project name and the
v0.6.0 environment. Keep retired credentials and the old listener out of the rollback.
Earlier binaries query tables that no longer exist. Returning to them requires
coordinated database and configuration restoration.
