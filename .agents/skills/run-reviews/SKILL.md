---
name: run-reviews
description: Dispatch the independent platform verifier and the reviewers required for the committed branch diff before opening or updating a PR.
---

# Run independent reviews

1. Record the development session's effective model and the reasoning setting reported by the host. Use a fresh `verifier` with the change and its success criteria. Let each role inherit the development model. Omit dispatch overrides and check the effective result in host events, the task inspector or the session transcript. Do not trust a role's self-description. Record the parent and child session identifiers, effective model and observable effort with the PR evidence. Accept the same model with equal or higher effort. Where the host has no separate effort setting, record its native inherited mode. A substituted model, reduced effort or unconfirmed inheritance requires another run with a verifiable configuration.
2. Fix verification failures and re-run the verifier. Commit the reviewed content. Determine the tier from the changed paths against the merge base. `AGENTS.md` defines the ordinary-documentation exception. Every review uses the same base/head and the base branch's `REVIEW.md` (`git show origin/main:REVIEW.md`). Supply the diff-triggered checklist and verifier evidence.
3. Open two fresh `reviewer` conversations for a regular change. Give each the original evidence. Keep their first passes independent of each other's findings. An ordinary documentation-only change may use one reviewer. In each dispatch use the native role by name and follow the same model and effort verification as for `verifier`.
4. Reproduce findings against the source. Fix confirmed defects, refute other findings with evidence, and carry earlier conclusions and fix commits into follow-up rounds. After a fix commit, each required reviewer must examine the changes through the final committed head. A behavior change also requires fresh verification. The PR records each verdict, evidence and outstanding limitation. An incomplete pass stays outstanding.
