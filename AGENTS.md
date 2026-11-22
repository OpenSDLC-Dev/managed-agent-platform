# Platform contributor instructions

## At session start

Read [STATE.md](./STATE.md) for active work. Check that this session received a **SessionStart hook** receipt containing `harness-doctor: OK` for this worktree. If it is missing, run `go run ./tools/harnessdoctor check` from the repository root and inspect project hook trust, instruction precedence and the active worktree. A manual check verifies the files. A fresh session's hook event verifies installation.

If the receipt reports drift, inspect the named paths. Edit the source under `harness/` and run `go run ./tools/harnessdoctor sync` followed by `check` in a writable task. In a read-only session, report the affected capability. Preserve unknown hooks and local settings. Start a fresh session after changing a registration.

Before working on a path, run `go run ./tools/harnessdoctor rules <path> ...` and read every matching file. This also works for proposed paths. The [harness guide](./harness/README.md) explains host permissions, sources and acceptance.

## Product decisions

The platform is self-hostable, Apache-2.0, with one open-source feature set. The first-class scenario is a general task agent.

1. Keep the public wire contract compatible with Anthropic Claude Managed Agents. Resolve public docs, the SDK at the pin in `go.mod`, then `ant` behavior and recordings. Record divergences and unconfirmed inferences in [DIVERGENCES.md](./docs/DIVERGENCES.md). The authority order and local reference paths are in [REFERENCE_PROJECTS.md](./docs/REFERENCE_PROJECTS.md).
2. Keep the domain Anthropic-native and stdlib-only. adk-go is a source of design ideas. Keep lossy conversions at the provider or gateway protocol boundary. New provider, sandbox and queue backends must pass their shared contract suites.
3. The event log owns session state. Brains are replaceable. Sandbox tools run asynchronously through the work queue. Propagate trace context across processes and emit public span events and OTel spans from the same instrumentation point.
4. Configure model providers, endpoints and credentials. A delegated claim token belongs only to its gateway route. An Anthropic-protocol provider must work with any endpoint that implements the Messages protocol.
5. The immutable initiator authorizes turns while they remain a member. Ownership metadata does not grant authority.

Merged migrations are immutable. New DDL belongs in a new numbered migration. Read [ARCHITECTURE.md](./docs/ARCHITECTURE.md) for as-built behavior.

## Decisions and evidence

- GitHub issues are the backlog. Assign an issue type (`Bug`, `Feature` or `Task`), one priority (`p0`–`p2`) and one difficulty (`easy`, `medium` or `hard`). Where information is missing, use `Feature`, `p1` and `hard` for the corresponding fields.
- For issue work, follow an existing applicable plan. Otherwise dispatch `issue-triage` with the issue and comments. Start easy work directly. Plan medium work first. Record the human decision before implementing hard work.
- Keep one numbered plan in `docs/plan/`. Change its frontmatter status in the PR that changes the work. Keep progress in `STATE.md`, including plan-less issue work. Archive a finished plan and record its acceptance or rejected decisions in `docs/HISTORY.md`.
- State the success criteria. For behavior changes, start with a failing contract test. Reproduce bugs with the real inputs when available. Ask the owner for unresolved product choices, decisions outside scope and destructive actions. Treat a request for assessment as a request for findings.
- After three consecutive failed repair rounds on the same check, stop and escalate with the command, output and hypotheses tried. Keep assertions, timeouts and fixture isolation meaningful.
- Run live or paid tiers only with explicit opt-in. The local ignored configuration supplies credentials. Snapshot existing processes and containers before verification, and remove only fixtures that this run created.

## Review and delivery

1. Work on a branch off fresh `main`. Each notable change gets a [changelog fragment](./changelog.d/README.md) in the same PR. Keep `README.md`, active plans and `STATE.md` current. Only the release workflow assembles or archives changelog sections.
2. Use the `verify` skill while developing. Before claiming a nontrivial result, dispatch the independent `verifier` with the change and success criteria. Its evidence includes `make verify`, real behavior, wire checks and documentation consistency.
3. Use the `run-reviews` skill on the committed diff. Load `REVIEW.md` from `origin/main` and apply its diff-triggered checklist. Run two independent `reviewer` sessions. A change consisting exclusively of ordinary documentation may use one. Changes to agent instructions, review rules, build or CI gates, harness sources or their adapters always need both reviews. Keep the verifier in every tier.
4. Verification and review use the development model with equal or higher observable reasoning effort. Record effective settings from the host for the parent and each child. Follow the skill when a host override lowers them or inheritance cannot be confirmed.
5. Reproduce findings, fix confirmed defects and respond to others with evidence. Reverify behavioral changes. Include verdicts and evidence in the PR. Squash merge only after all required checks pass and all review threads are settled, then sync local `main`. See [RELEASING.md](./docs/RELEASING.md) for releases.

Report what this run demonstrated. Identify blocked decisions, missing evidence and residual failures. Keep repository prose short and put each enduring fact in one place.
