---
status: archived
issue: 433
---

# Inference geo protocol compatibility

## Contract and scope

The user chose a deployment with one operator-configured region and requested
protocol compatibility only (2026-09-22). Preserve the requested field as model
configuration metadata; do not use it to select a region or send a constraint to
providers. Actual inference geography remains the operator's upstream configuration.
An echoed value records the request, not evidence of geographic enforcement.

1. Accept optional `model.inference_geo` as `"us"` or `"global"` and echo it as a
   string. Omission or null stays absent; do not invent a workspace default. Reject invalid
   selections and types. Sources: the public
   [agent guide](https://platform.claude.com/docs/en/managed-agents/agent-setup#pin-the-inference-geo)
   (read 2026-09-22); checked against anthropic-sdk-go v1.70.1 — betaagent.go
   BetaManagedAgentsModelConfigParams.InferenceGeo and BetaManagedAgentsModelConfig.InferenceGeo.
   The same tag's [OpenAPI schema](https://github.com/anthropics/anthropic-sdk-go/blob/v1.70.1/scripts/mock-spec.json.gz),
   `components.schemas.BetaManagedAgentsModelConfigParams.properties.inference_geo`,
   permits null input; the response field is an optional non-null string.
2. Omitting `model` from an agent update preserves the saved configuration.
   Supplying `model` replaces its geo field: omission or null clears it, including a bare
   model ID or the same ID. Preserve immutable agent versions.
3. A session without a model override inherits its pinned agent version. A supplied
   model replaces the geo value or clears it on omission or null, without changing the base
   agent. The JSONB model snapshots need no migration. Source: the public
   [session guide](https://platform.claude.com/docs/en/managed-agents/sessions#pin-the-inference-geo-for-a-session)
   (read 2026-09-22); checked against anthropic-sdk-go v1.70.1 — betasession.go
   BetaManagedAgentsAgentWithOverridesParams.Model.
4. No provider request, routing, workspace geo policy, model capability check or
   roster-wide geo consistency check is added. These are deliberately outside the
   user's selected compatibility scope and are recorded in DIVERGENCES and README.
   The narrower dream model contract is unchanged.

## Verification

- Reproduce the lost field with an HTTP create/read test before implementing it.
- Cover both values, absent and null fields, malformed selections, update replacement,
  immutable versions, pinned sessions and session override persistence; decode a
  response with the pinned reference SDK.
- Exercise real controlplane and brain binaries with PostgreSQL and HTTP upstream
  fixtures, checking persisted geo metadata and unchanged requests to both protocols.
- Run `make verify`, the registry and SDK checks, independent verification, both
  reviews and the PR CI gates before squash merge.
