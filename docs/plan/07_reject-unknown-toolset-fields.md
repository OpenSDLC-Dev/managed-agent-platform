---
status: archived
issue: "#26"
---

# Reject unknown fields in an agent_toolset_20260401 config

> Archived 2026-07-22: completed. Delivered in one PR
> ([#151](https://github.com/OpenSDLC-Dev/managed-agent-platform/pull/151)); the delivery record is in
> [docs/HISTORY.md](../HISTORY.md) § "Reject unknown agent_toolset fields (plan 07)", the narrative in
> CHANGELOG.md. **Everything below describes the state of the repository *before* that PR** — read it
> as the argument for the change, not a description of the result.

The plan for [#26](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues/26).

## The defect

`internal/toolset/definitions.go` decodes an `agent_toolset_20260401` entry with a plain
`json.Unmarshal` into the `entry` / `policyConfig` structs. Go's JSON decoder silently drops
unknown object keys, so a **misspelled** `permission_policy` — the issue's `permission_polciy` —
is discarded, `PermissionPolicy` stays nil, and `resolveToolset` falls through to
`DefaultAgentToolsetPolicy`, which is `always_allow`. An operator who wrote `always_ask` to require
human confirmation instead gets automatic execution: a **fail-open at the human-in-the-loop
approval boundary**. `internal/api/wire.go`'s `parseTools` validates the entry with `toolset.Validate`
and then stores the raw object verbatim, so the malformed object is accepted and persisted.

The existing tests reject a bad *value* inside a correctly spelled `permission_policy`
(`TestPoliciesRejectsUnknownPolicy`) but never a *misspelling of the field name itself*.

## Why this needs a plan file

The fix hinges on **wire-schema verification** — the issue's own proposed fix step 1 is "derive
accepted fields from the pinned `anthropic-sdk-go` schema." Per
[`.claude/agents/issue-triage.md`](../../.claude/agents/issue-triage.md) and CLAUDE.md's
wire-compatibility rules, that resolution belongs in a plan, pinned against the SDK version in
`go.mod` (v1.58.0), not improvised mid-implementation — the diff size (a single new validator) is
explicitly not the test. The registry of accepted keys below is the artifact the verifier's
wire-compat rung and reviewers check against.

## The pinned wire schema

Accepted keys at each nesting level of an `agent_toolset_20260401` tool, taken field-for-field from
`anthropic-sdk-go` v1.58.0's **request** (`*Params`) types in `betaagent.go`. Any other key is a
typo or an invented field and must be rejected before the entry is stored.

| Object | Accepted keys | SDK source type |
| --- | --- | --- |
| toolset object | `type`, `configs`, `default_config` | `BetaManagedAgentsAgentToolset20260401Params` |
| `default_config` | `enabled`, `permission_policy` | `BetaManagedAgentsAgentToolsetDefaultConfigParams` |
| each `configs[]` entry | `name`, `enabled`, `permission_policy` | `BetaManagedAgentsAgentToolConfigParams` |
| `permission_policy` | `type` (enum `always_allow` \| `always_ask`) | `BetaManagedAgentsAlwaysAllowPolicyParam` / `…AlwaysAskPolicyParam` |

An omitted `permission_policy` is legitimate and keeps the documented default; the fix only stops a
*misspelled* key from being read as an omission. The `always_allow` default itself is unchanged, so
`docs/DIVERGENCES.md`'s INFERRED entry for it (#59) is untouched — this is not a new divergence.

## The fix

A recursive, path-aware unknown-key check inside package `toolset`, mirroring the API's existing
`rejectUnknownKeys` pattern and run as a step of `resolveToolset` (so `Validate`, `Tools`, and
`Policies` share one strict path with no drift, and every API and brain caller is fail-closed).
`json.Decoder.DisallowUnknownFields` on the current `entry` struct is rejected: `entry` omits the
top-level `type` discriminator, so strict decoding it unchanged would reject valid input, and the
stdlib error names no field path.

The check is **eager** — it rejects an unknown key regardless of a tool's enable state, because a
malformed *stored object* is the defect (a typo on a disabled tool is a latent fail-open that
activates when the tool is enabled). This is orthogonal to the existing **lazy** policy-*value*
validation (`TestPoliciesValidatesLazily`, which uses correctly spelled keys), so the two coexist.

`parseTools` is the single chokepoint for all three API paths — agent create/update
(`internal/api/agents.go`), session create `agent_with_overrides` and session update `agent.tools`
patch (`internal/api/sessions.go`) — so one fix closes every path, each returning HTTP 400
`invalid_request_error` before persisting.

## Acceptance criteria → coverage

- `toolset.Validate` rejects `default_config.permission_polciy` and `configs[n].permission_polciy`,
  naming the field's path — new `internal/toolset` unit tests.
- Unknown keys at every nesting level (toolset object, `default_config`, `configs[]`,
  `permission_policy`) rejected, including on a disabled tool — new unit tests.
- Each API path (agent create, session create overrides, session update patch) returns 400 and does
  not persist — new `internal/api` tests.
- A correctly spelled `always_ask` still resolves to `always_ask`, and a genuinely omitted policy
  keeps the default — retained by existing `TestPolicies` / `TestPoliciesValidatesLazily`.
- A valid `always_ask` turn enqueues no tool work before confirmation — already asserted by
  `TestConfirmationClosedLoopAllow` (`liveWork(ToolExec) == 0` before confirm).
- `make verify` green (build, crossbuild, vet, fmt, test, ≥90% coverage).
