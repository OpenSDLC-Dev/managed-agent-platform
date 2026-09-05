---
status: archived
issue: "#353"
---

> **Archived — completed.** Delivered in one PR, closing #353 and #576 together; the
> reference's cross-session install cache (decision 2) is deferred to #595, and the
> gate's package-registry allow-set stays #591. The progress summary is in
> docs/HISTORY.md.

# Environment `config.packages` installed into the sandbox (plan 40)

`POST /v1/environments` accepts, validates, stores and echoes `config.packages` — six
manager lists, `apt`/`cargo`/`gem`/`go`/`npm`/`pip` — and nothing downstream reads them. The
executor loads the environment's whole config for every run (`sessionForRun`) and reads it
for networking and the MCP passes only; `sandbox.Spec` has no packages field; the sandbox
is created from the deployment-wide `EXECUTOR_IMAGE` and nothing is ever installed into
it. A client following the reference docs gets a 200 environment, a 200 session, and an
agent whose first `import pandas` raises (#353, re-confirmed 2026-09-03 against a compose
stack: `pip: [cowsay==6.1]`, `apt: [jq]` → `ModuleNotFoundError`, `no-jq`). Two of
Anthropic's own cwc-workshops (`research-desk`, `production-ready-agent`) fail on this
platform for exactly this reason.

The issue offers three ends: implement the pre-install, refuse a non-empty `packages`
with a 4xx, or document that the field is storage-only. This plan takes the first. The
field is part of the wire the platform promises to serve, its semantics are documented,
and refusing what the reference accepts would break the same clients the silence does —
only louder.

## 1. Scope and goal

A cloud environment's `config.packages` is installed into every session sandbox provisioned
for it, before the session's first tool runs, by the managers the reference names, in the
order it names them; a manager that cannot install surfaces as a `session.error` the client
can read, and the session runs on. An environment that lists packages under `limited`
networking without `allow_package_managers` is refused at create/update with a 400, as the
reference refuses it (#576 — the create-time half of the same feature, folded in here
because an install that can only be refused at the gate is better refused at the request).

Delivered in one PR that closes #353 and #576 together and re-points the registry entry
that names the latter (slice 3). Not delivered: the reference's cross-session install
cache (decision 2), the gate's package-registry allow-set (#591), and anything for
`self_hosted` environments (decision 8).

## 2. Ground truth (pinned 2026-09-05)

**The reference docs** (platform.claude.com managed-agents/environments, "Packages"):
"The `packages` field pre-installs packages into the sandbox before the agent starts.
Packages are installed by their respective package managers and cached across sessions
that share the same environment. When multiple package managers are specified, they run in
alphabetical order (apt, cargo, gem, go, npm, pip). You can optionally pin specific versions.
Unpinned packages install the latest version. If the environment uses `limited` networking,
also set `networking.allow_package_managers` to `true`; otherwise the request is rejected
with a 400 error." The supported-managers table gives one pin example each: `apt`
`"graphviz"`, `cargo` `"hyperfine@1.18.0"`, `gem` `"rails:7.1.0"`, `go`
`"golang.org/x/tools/cmd/goimports@latest"`, `npm` `"express@4.18.0"`, `pip`
`"sqlalchemy==2.0.30"` — each the manager's own native syntax, so an entry is passed to its
manager verbatim. The networking section adds that the flag must be set "whenever the
environment specifies `packages`; otherwise the request is rejected with a 400 error, even
if the registry hosts are listed in `allowed_hosts`". The page says nothing about what
happens when an install fails, how long one may take, what a `packages` list may contain, or
whether a package change reaches a session already running.

**The SDK** (anthropic-sdk-go v1.66.0, betaenvironment.go): `BetaPackages` is six
`[]string` fields plus the `type` discriminator #382 settled; the doc comment on the params
says "You are responsible for validating the package and version exist" (:270, :618 —
echoed by the `ant` CLI's flag help), which is about existence, not syntax. Its
session-error union (betasessionevent.go) has eight variants — `billing_error`,
`credential_host_unreachable_error`, `mcp_authentication_failed_error`,
`mcp_connection_failed_error`, `model_overloaded_error`, `model_rate_limited_error`,
`model_request_failed_error`, `unknown_error` — none about packages; every variant's
`retry_status` is one of `{type: "retrying"}`, `{type: "exhausted"}` and
`{type: "terminal"}`, and nothing else. Whatever a
failed install looks like on the reference wire is unrecorded; this platform's variant will
be its own, as `github_repository_clone_error` (plan 25) is.

**This platform.** `internal/api/environments.go` `parsePackages` validates manager names
and that each list is a JSON string array, and nothing about the strings; every stored
cloud config carries all six lists, empty or not. `domain.EnvironmentConfig.Packages` is
`map[string][]string`. `internal/executor` `provisionAndRun` provisions, then materializes
skills, repositories, files and memory stores in that order, each a tolerated pass with
`progress()` between steps (#383) and its own span and duration metric; `provisionSandbox`
takes the session advisory lock for the provision and restore and releases it on return.
`sandbox.Sandbox.Exec` runs one `bash -c` command in the sandbox as the container's user
with a caller-chosen timeout, keeps the first `MaxOutputBytes` (1 MiB) of each stream and
discards the tail, and on both backends enforces the deadline by SIGKILLing the command's
process group. The container is created from `spec.Image` with a `bash` hold-open
entrypoint; `Hardening.ReadOnlyRootfs` leaves only `WritablePaths` writable and
`Hardening.RunAsUser` runs everything as that uid — both bound at create and not re-applied
to an adopted sandbox. A `limited` session's egress goes through its gate where one is
configured, which admits `allowed_hosts` and the MCP endpoints and (until #591) nothing for
`allow_package_managers`; where no gate is configured — every Kubernetes deployment, and a
Docker deployment that has not opted in — a `limited` sandbox has no route out at all. The
queue admits one live `tool_exec` per session, but a reclaim can overlap the lapsed holder's
still-running pass. The checkpoint (plan 24) preserves the workdir, the shell state root
and the outputs root; `/tmp` is deliberately not preserved. `StallTimeout` bounds an item's
silence and is floored above the longest single step by `toolset.StallFloor`, whose callers
today name exactly one step (`RepoCloneTimeout`). Event payloads are `jsonb`, which cannot
hold a NUL byte — `toolset.SanitizeText` and `toolset.TruncateRunes` are the platform's
last stop before it, and the brain bounds a `session.error` message at 8 KiB
(`maxFailureMessage`).

## 3. Design decisions

**D1 — Implement, in the executor, through `Sandbox.Exec`.** The install is a
materialization pass like skills, repositories and files: after the sandbox is provisioned
and before `materializeSkills`, one `Exec` per non-empty manager, in the reference's
alphabetical order. It touches neither backend: the sandbox is created exactly as today and
the commands run through the seam every tool already uses, so Docker and Kubernetes behave
identically without a contract-suite change, and `sandbox.Spec`'s adoption-compared fields
(`Image`, `Networking`) are untouched. The alternative — a per-environment image built from
the packages and cached — is the reference's shape (its "cached across sessions" sentence
can mean nothing else) and is rejected here for the delivery cost: Kubernetes has no image
build primitive, Docker's would need a registry both executors and the chart trust, and the
image would join the adoption comparison. That cache is the follow-up this plan defers, not
a slice of it.

**D2 — Per sandbox, serialized, and remembered.** Packages install into each session's
sandbox at its first provision; a second session on the same environment installs again.
The reference caches across sessions; this platform does not, and registers the
difference. The pass runs *inside* the session advisory lock `provisionSandbox` already
holds — on the tool-run path only; the harvest's provision installs nothing — so a
reclaiming executor waits on the lapsed holder's pass instead of racing its `apt-get` for
the dpkg lock, with no sandbox-side lock the image would have to supply. A sandbox's later
provisions (every `tool_exec` after the first, a restore after a reap) do not repeat what is
already settled: a sentinel at `/tmp/.map-packages` — under `/tmp` because it is writable
in every hardening shape and *not* in the checkpoint, so a restored sandbox, which is a
fresh container, installs again — records, per manager, the list last attempted, whether it
installed, and how many attempts that list has had. A manager whose list equals the recorded
one and installed is skipped; a changed list is a new attempt with a fresh count; a manager
whose list matches but failed is retried at most **three** times per sandbox, after which
it is skipped until its list changes — a typo'd entry or a registry the gate refuses stops
costing a ten-minute install before every tool call, and a transient failure still gets a
second chance. The sentinel is written through `WriteFile`, which is atomic, and is
agent-writable and trusted for nothing load-bearing: a forged one skips an install the
agent then lacks, a deleted one costs a repeated install. An install's idempotence is a
property of a command that ran to completion, not of one the deadline killed: D3's `apt`
command opens with the repair that makes a killed transaction recoverable.

**D3 — The commands.** One `bash -c` string per manager, every entry single-quoted
(`shellQuote`) so a list element is one argv member whatever it contains, and passed to its
manager verbatim as the reference's table shows — with one exception, `go`, below. Each
command runs under `set -o pipefail` as `{ <preflight>; <install>; } 2>&1 | tail -c 8192`,
so the bytes the pass keeps are the *last* 8 KiB of the combined output — the failure —
rather than the head `Exec` would keep of a stream past its cap. The preflight is
`command -v <manager> >/dev/null 2>&1 || exit 127` (for `pip`, `python3 -m pip --version`,
since a slim image ships `python3` without `pip` and that failure exits 1), so a missing
manager is exit 127 whatever the manager:

| manager | install |
| --- | --- |
| `apt` | `export DEBIAN_FRONTEND=noninteractive; dpkg --configure -a; apt-get -o APT::Sandbox::User=root update -q && apt-get -o APT::Sandbox::User=root install -y -q <entries>` — the `dpkg --configure -a` repairs a transaction an earlier deadline killed, which otherwise wedges every later `apt-get`, the agent's own included; `APT::Sandbox::User=root` keeps apt's acquire methods as root, since dropping them to `_apt` takes the `CAP_SETUID`/`CAP_SETGID` the platform's own default capability drop removes and every fetch would die |
| `cargo` | `cargo install --root /usr/local <entries>` |
| `gem` | `gem install --no-document <entries>` |
| `go` | `GOBIN=/usr/local/bin go install <e1> && … <eN>` — one invocation per entry, because `go install` refuses `@version` arguments from different modules in one call; an entry carrying no `@` gets `@latest` appended, because outside a module `go install` requires a version and the docs promise an unpinned entry installs the latest |
| `npm` | `npm install -g <entries>` |
| `pip` | `PIP_BREAK_SYSTEM_PACKAGES=1 PIP_DISABLE_PIP_VERSION_CHECK=1 PIP_NO_INPUT=1 python3 -m pip install <entries>` |

The choices that are ours are registered: binaries from `cargo` and `go` land in
`/usr/local/bin` rather than a home directory no `PATH` in an arbitrary image includes;
`npm` installs globally, so a package's binaries are on `PATH` while `require()` from the
workdir needs a `NODE_PATH` the sandbox does not set; `pip` overrides PEP 668's
externally-managed refusal — the environment variable rather than the flag, because an
older pip rejects an unknown flag and ignores an unknown variable — since the platform *is*
the environment's manager here; `apt-get update` precedes the install because a slim image
ships no package lists; and the `go` `@latest` suffix. The install runs as the container's
user and needs root and a writable root filesystem — what the default image and the
reference's sandbox both provide, and what D7 checks.

**D4 — Failure is surfaced, not fatal, and it retries a bounded number of times.** A
manager's install that exits non-zero, times out, or is refused before it starts records
one `session.error` event whose `error` object is
`{type: "environment_package_install_error", manager, reason, message, retry_status}`:
`reason` one of `failed` (non-zero exit), `manager_missing` (exit 127), `timeout`,
`invalid` (an entry the platform refuses to pass, D6), `sandbox_not_root` and
`rootfs_read_only` (D7, which carry no `manager` — they are the sandbox's); `message` the
kept tail of the manager's own output (D3), passed through `toolset.SanitizeText` — a NUL
byte would fault the append, and a faulted item reclaim-loops — cut with
`toolset.TruncateRunes` at the same 8 KiB the brain's `maxFailureMessage` holds, and with
the userinfo of anything URL-shaped (`scheme://user:secret@host`) replaced by `***@`,
because a `pip` or `npm` entry may legitimately be a URL carrying a credential and a manager
echoes its arguments. The entry list is deliberately **not** carried on the event: the
environment's config already holds it for a management key, while the events subtree is
also readable with an environment key, and the manager plus the output tail name what
failed. This is the one `session.error` this platform writes whose text is
sandbox-controlled rather than fixed (the clone error's is fixed; the MCP path cuts its
message to `scheme://host`), and it is a deliberate choice — the output *is* the diagnosis,
and the sanitize, bound and redaction above are the argument for carrying it. Deduped on
(manager, reason, retry_status.type): a repeated identical failure is one event, a reason
flip is a new one, and the third failed attempt of one list (D2) re-emits with
`retry_status: {type: "exhausted"}` where the earlier ones said `retrying`; the sandbox-level
reasons and `invalid` are `exhausted` from the first, since nothing in the session's life
changes them. The session runs on — its other managers still install, its tools still run —
and the agent meets the missing package the way it would on a host: an import error it can
read. A backend error from `Exec` (the sandbox gone, the context cancelled) faults the
pass and the item, as a provision failure does. Fatal was rejected because a typo in one
manager's list would park the session forever (a faulted item reclaim-loops); silent was
rejected because it is the bug.

**D5 — Budget and progress.** One manager's install is one step under the stall bound:
`Config.PackageInstallTimeout` (`EXECUTOR_PACKAGE_INSTALL_TIMEOUT`, default 10 minutes —
`toolset.MaxTimeout`, the longest step the default stall budget already clears) is the
`Exec` timeout per manager. It joins `RepoCloneTimeout` in the floor the stall budget is
held above through a small tested helper in `internal/toolset` that picks the longest of
the named steps and its knob (`ParseStallTimeout` and `CheckStallDefault` today take one
step and one name; the selection stays inside the coverage gate rather than in `main`, for
the reason #383 moved the guard out of `main`), so an operator raising the install knob for
a heavy `apt` list cannot create the reclaim loop plan 33 exists to prevent. `progress()`
is reported before each manager's `Exec`, as the other passes report per item. The pass has
its own span (`packages_install`, attributes for managers run, skipped and failed) and
duration metric, matching its siblings.

**D6 — What an entry may be.** The API refuses, with a 400 naming the manager, an entry
that is empty or begins with `-`: the former is meaningless to every manager and the latter
is an option, not a package — `--index-url=…` handed to `pip install` is the injection the
quoting in D3 cannot stop, because the string *is* a single argument. Nothing else is
refused: whitespace, `@`, `==`, `:` and the rest are the managers' own syntax and are
quoted whole. The executor applies the same predicate before building a command, so a row
stored before this rule (reason `invalid`) is refused at install rather than passed. The
predicate lives in `internal/domain` beside `EnvironmentConfig`, so the two callers cannot
drift. The reference's entry validation is unrecorded, and the SDK's "you are responsible
for validating the package and version exist" points the other way — a create-time 400 on
an entry the reference may accept is a divergence in the strict direction, registered as
such with that sentence as the evidence.

**D7 — The sandbox must be root with a writable root.** Every manager writes under `/usr`
or `/var`. Before the first manager that actually runs — the probe is lazy, so a pass
whose managers are all settled or refused costs no exec at all — one cheap probe in the
sandbox itself — `id -u`
and whether `/usr` and `/var` are writable — refuses the whole pass with reason
`sandbox_not_root` or `rootfs_read_only`, recorded once (D4) and running no manager,
rather than six timeouts' worth of `Permission denied`. An answer the probe cannot have
produced — a shell so broken it printed something else — installs anyway, with a log line
rather than an invented wire reason: the install's own failure is then the honest
diagnosis. Probing the sandbox rather than
reading `cfg.Hardening` is deliberate: `RunAsUser` and `ReadOnlyRootfs` are bound at
create and an adopted sandbox may disagree with the executor's current config, and an image
whose *own* default user is non-root is invisible to the config entirely. The combination
— `config.packages` with `SANDBOX_RUN_AS_USER` or `SANDBOX_READONLY_ROOTFS` — is
documented in docs/self-hosted-security.md §2 and §4 beside the knobs.

**D8 — `cloud` only.** A `self_hosted` config has no `packages` field (the API already
refuses one), and the BYOC worker owns its own sandbox image. Nothing changes in
`internal/worker`.

**D9 — The create-time 400 (#576).** After `normalizeEnvConfig` has parsed both blocks
onto the merged base, a cloud config whose `networking.type` is `limited`, whose
`allow_package_managers` is not `true`, and any of whose six lists is non-empty is refused:
400 `invalid_request_error`, message
`packages require networking.allow_package_managers to be true under limited networking`.
The docs' sentence — "whenever the environment specifies `packages`" — admits a second
reading, key presence; this plan takes the non-empty one, because every stored cloud config
carries all six lists and the reference's own response type marks `packages` required, so
"specifies" can only mean lists with entries. The predicate and the message are both ours
and both registered. The check runs on the *merged* result, so an update that adds packages
to a limited environment, or switches a packaged environment to `limited`, is refused the
same way a create is — and so is any config patch on a `limited` + packages environment
stored before this rule, until the operator sets the flag or clears the lists, which the
message names.

**D10 — Timing and the running session.** The install happens at the session's first
sandbox provision, which on this platform is its first `tool_exec` — the lazy lifecycle
plan 25 already recorded for clones — where the reference installs "before the agent
starts". A `packages` update on the environment reaches a running session at its next
provision (D2's sentinel comparison), which the docs neither promise nor forbid.

**D11 — Networking.** Under `limited` the install's egress is the session's: through the
gate where one is configured, which until #591 refuses a registry reachable only by
`allow_package_managers` (reason `failed`, the proxy's 403 in `message`), and nowhere at
all where none is — every Kubernetes deployment, and a Docker deployment without a gate
image, give a `limited` sandbox no route out, so there `allow_package_managers: true`
buys nothing and the install fails the same bounded way. `allowed_hosts` naming the
registry works today on a gated deployment. Visible, never silent, and nothing here widens
the gate.

## 4. Out of scope

- The reference's cross-session install cache (a per-environment image keyed by the
  packages hash) — a follow-up issue, filed by the delivering PR.
- #591, the gate's package-registry allow-set.
- BYOC (`self_hosted`) package installation.
- Any `packages.type` question — settled by #382/#583.

## 5. Slices (one PR)

1. **The create-time 400** — `normalizeEnvConfig` cross-check (D9) and the entry predicate
   (D6) in `internal/domain`, applied by `parsePackages`. Tests: create and update, each
   refusal and the `allow_package_managers: true` acceptance; a legacy `limited` +
   packages row refuses a patch that touches only `allowed_hosts` and names the remedy;
   empty and `-`-prefixed entries refused per manager; the existing
   `TestEnvironmentCreateSelfHostedAndLimitedCloud` fixture gains the flag it now needs.
2. **The install pass** — `internal/executor/packages.go`: the commands (D3), the
   sentinel with its attempt count (D2), the error variant, its sanitizing and its dedupe
   (D4), the timeout and the toolset floor helper (D5), the probe (D7), the span and
   metric, called under the advisory lock on the tool-run path. Unit tests on the fake
   sandbox (which gains an error-returning exec seam for the backend-fault arm): order,
   quoting, the `go` suffix, per-manager skip, reinstall on a changed list, the three-
   attempt cap and the `exhausted` re-emission, every reason, dedupe, the sentinel path
   lying under none of the checkpoint roots — which is what makes a restored sandbox
   install again — no-op on an empty config, the harvest path installing nothing. Two
   real-sandbox tests beside `TestClosedLoopRealSandbox`: in the default gate, a stub
   `apt-get` planted at `/usr/local/sbin` of a pre-provisioned `debian:stable-slim`
   sandbox records the argv the pass hands it and the sentinel it leaves — the seam
   end-to-end with no public-internet dependency, which no test under `internal/` has
   and `make test` must not gain; and behind `RUN_LIVE_PACKAGE_TESTS=1` (a consent-only
   tier, added to README's table) the real `apt: [jq]` install on that image, then a
   `bash` tool call proving `jq` answers — the acceptance the issue asks for.
3. **Docs** — docs/DIVERGENCES.md: new INFERRED entries for D2, D3, D4, D6, D9, D10,
   each naming #78 as its live tracker with a parenthetical (the entries' own issues are
   closed by this PR and cannot be trackers), and the egress-gate entry amended — its
   create-time sentence to the past tense, #576 moved from the `Tracked:` head to the
   `landed for` tail — so `make registry-check` stays green; docs/ARCHITECTURE.md's
   execution-flow step 4; docs/self-hosted-security.md §2 and §4 (D7);
   `changelog.d/environment-packages.added.md` and `.fixed.md` (the #576 half), and the
   closing sentence of `changelog.d/packages-type-key.fixed.md`, which says the runtime
   half stays open and will no longer be true in the same release; this plan archived,
   its progress summary in docs/HISTORY.md, STATE.md's line; the follow-up issue for the
   cache.

## 6. Acceptance

The issue's own criterion: a cloud environment with `packages.apt: [jq]` (and, on an image
that carries pip, `packages.pip: [cowsay==6.1]`) yields a session whose first `bash`
call finds the package — proven by the live-tier test in slice 2 and, before merge, by a
compose-stack run of the 2026-09-03 reproduction with its outcome recorded in the PR.
