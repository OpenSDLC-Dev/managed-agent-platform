---
status: archived
issue: "#196"
---

# Check the gated sandbox process identity

A gated sandbox must run its image and every tool as a different UID from the session's egress gate. Plan 19 checks an operator's numeric `RunAsUser` before provisioning. When the operator leaves it unset, the runtime selects the image's user. That user may be named, and the gate shares its network namespace with the sandbox. The identity decision belongs before any image program runs.

## Decisions

- Keep the image's user and group selection. Check the effective UID reported by the kernel in a small, statically linked launcher supplied by the trusted gate image. A match with the platform's reserved gate UID exits with a recognizable refusal. An explicit safe override continues to work for an image whose default conflicts. Ungated sandboxes keep their existing startup path.
- Give the sandbox a read-only mount containing the launcher. Docker sources it from the session's gate volume. Kubernetes uses a pod-local volume populated by the gate before its startup probe succeeds. The sandbox image supplies neither the executable nor the value against which it checks. A missing launcher fails closed.
- Start the sandbox through the launcher, keep image-supplied startup or health hooks behind admission, and verify readiness before returning a handle. Guard later executions that could resolve an image account again. Creation, adoption, a concurrent create winner and attach must apply the same invariant. Refuse an unverified existing sandbox without discarding its workspace.

## Acceptance

The Docker and Kubernetes provider contracts use an image whose default user resolves to the gate UID. Provisioning must refuse it before handing out a usable sandbox. The same fixture with a safe explicit override must start at that UID. Cover a named image user, a safe image default and reattachment. These checks need only observe admission and process identity. Existing gate connectivity and hardening tests cover the surrounding policy.
