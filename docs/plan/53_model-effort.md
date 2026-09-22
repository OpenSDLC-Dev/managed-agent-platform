---
status: archived
issue: 160
---

# Model effort

## Contract and decisions

1. Accept `model.effort` as a level string or `{ "type": level }`, for
   `low`, `medium`, `high`, `xhigh`, and `max`; render an explicitly selected
   level as an object. Reject malformed values at the API boundary. Source:
   checked against anthropic-sdk-go v1.70.1 — betaagent.go
   BetaManagedAgentsModelConfigParams.Effort and BetaManagedAgentsModelConfigEffortUnion.
2. Agent updates that omit effort retain the saved level only for the same
   model ID; changing the ID resets to the new model's default. The public
   [agent guide](https://platform.claude.com/docs/en/managed-agents/agent-setup#update-semantics),
   read 2026-09-22, refines the shorter SDK comment. Session model overrides
   replace the model configuration; omitting the whole model inherits the
   pinned agent's selection. The guide says the reference ignores session-level
   effort; this platform deliberately honors an explicit override, matching
   #160's requirement to propagate the selection rather than silently drop it.
   Register that extension alongside the backend-default differences below.
3. Preserve the existing config-driven backend policy: do not infer a model's
   capabilities or default from its client-facing name (a wildcard route may
   map that name to another model). Omitted effort stays absent and uses the
   upstream default; validate the enum locally and leave model-specific support
   to the endpoint. This deliberately differs from the reference's save-time
   default resolution and model/effort validation, and belongs in DIVERGENCES.
4. Carry the resolved level on every model turn, including child-agent and
   outcome-grader turns. Anthropic requests use `output_config.effort`:
   checked against anthropic-sdk-go v1.70.1 — message.go OutputConfigParam.Effort.
   OpenAI-compatible requests use `reasoning_effort`, preserving the level:
   [Chat Completions reference](https://developers.openai.com/api/reference/resources/chat/subresources/completions/methods/create),
   read 2026-09-22, lists all five levels. Endpoint errors remain visible; the
   platform neither drops nor downgrades an explicit selection.
5. Existing JSONB snapshots need no migration. Legacy snapshots without effort
   keep the upstream default. Geography support remains separate (#433).

## Verification

- Reproduce lost effort through domain JSON round trips and HTTP agent creation.
- Exercise all input levels/forms, invalid values, omission, agent updates,
  immutable versions, session overrides and persisted snapshots.
- Capture real adapter HTTP requests for both protocols and exercise brain,
  child-agent and grader request propagation.
- Run `make verify`, `make registry-check`, `make sdk-bump-report`, independent
  verification, both review passes, and all PR CI before squash merge.
