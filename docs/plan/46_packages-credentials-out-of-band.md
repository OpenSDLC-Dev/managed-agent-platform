---
status: archived
issue: 599
---

# Plan 46 — a package-manager credential leaves argv

A `config.packages` entry may carry its own credential, because a package
reference is allowed to be a URL: `pip: ["git+https://user:token@host/repo"]`,
`npm: ["https://user:token@host/pkg.tgz"]`. Plan 40 (#353) closed the durable
surface — `/tmp/.map-packages` stores a digest rather than the entries — and
left two residuals, which #599 tracks and this plan closes.

1. **The install command is one `bash -c` string, and it goes in argv.** Both
   backends put it there: the docker backend as `execConfig.Cmd`, the k8s
   backend as the exec subresource's `command` parameters — which the apiserver
   records wherever auditing covers `pods/exec`. That second one is the only
   half of this issue that leaves the sandbox: a cluster operator reading an
   audit log is not in the session's trust domain, and the credential reaches
   them.
2. **`packages_digest` is an unsalted oracle.** It is `SHA-256` over the
   entries, and it rides on the `environment_package_install_error` event,
   whose subtree an *environment* key can read while the environment config it
   digests needs a *management* key. A holder who knows the list's shape but
   not a weak credential in it can brute-force the credential offline.

## What each manager's transport actually honours

The design is "put the credential in the file the tool reads instead of the
command line", so which file, per transport, is the load-bearing fact. Measured
2026-09-06 against a local HTTP origin that demands Basic auth and records what
arrives; none of it is recalled from documentation.

| transport | file written | what the origin saw |
|---|---|---|
| `git` 2.47.3 (pip's and npm's `git+https://`) | `$HOME/.netrc` | `Authorization: Basic`, on the retry after the 401 |
| `pip` 25.0.1's own fetcher (a direct archive URL) | `$HOME/.netrc` | `Authorization: Basic`, on the **first** request |
| `npm` 11's own fetcher (a tarball URL) | `$HOME/.npmrc`, per-host `username` + `_password` (base64) + `always-auth` | `Authorization: Basic`, on the first request |

Two properties of the netrc format were measured with it, because both decide
what the writer may emit: a `machine` line matches on the **hostname alone**,
port excluded, and a value may be **double-quoted**, with `\"` and `\\` honoured
inside the quotes. So a credential containing spaces or quotes has a
representation; one containing a newline does not.

## Why not the vault placeholder instead

The obvious alternative is to leave credentials out of `config.packages`
entirely and let the gate's egress substitution put them in: an entry would
carry `{{vault.…}}`, and no secret would reach argv at all. It does not work
here, and the reason is structural rather than a gap to close: the gate
substitutes **only on plain HTTP**, where the platform holds the request
plaintext. An HTTPS request rides through as an opaque CONNECT tunnel, admitted
or refused on its host and never inspected (#166) — and a package registry is
HTTPS. The placeholder would reach the origin literally.

## Decisions

1. **Extraction is by URL shape, not by manager.** An entry is credential-bearing
   when it parses as a URL whose authority carries userinfo *with a password* —
   `scheme://user:secret@host/…`, the `git+https` composite included. A bare
   `user@host` is not a credential and is left alone. Everything else about the
   entry is untouched, and a non-URL entry is not rewritten at all. Doing this
   per manager instead would mean six near-identical rules, and the two the
   issue names (pip, npm) are only where the shape is *common*, not where it is
   possible.
2. **The credential is materialized into a scratch `HOME`, and the install for
   that manager runs with `HOME` pointed at it.** `$HOME/.netrc` always;
   `$HOME/.npmrc` additionally for the npm manager, whose fetcher reads no
   netrc. The alternative — writing into the image's own `/root` and restoring
   afterwards — needs a read-modify-write and a restore path that can leave a
   credential behind when it fails. A scratch directory has neither, and its
   removal is unconditional. The cost is stated rather than hidden: for that one
   install, an image that ships its own `~/.npmrc` or `~/.netrc` is not read,
   and the manager's `HOME`-rooted cache is cold. Both apply only to a manager
   whose list actually carries a credential.
3. **The directory name is random** (`sandbox.TempName()`, the name the atomic
   write already uses). The sandbox is agent-writable, so a fixed path could be
   pre-created as a regular file by the agent to make the write fail; a random
   one cannot be waited for.
4. **The removal rides on the install command's own `trap … EXIT`**, so a
   manager that fails, times out, or exits non-zero still cleans up. The trap
   carries a path and no secret, so it is argv-safe. What it does not survive is
   the executor dying between the write and the exec: the file then lives as
   long as the sandbox does, which is until the session's sandbox is discarded.
   The next pass's write is to a fresh random directory rather than that one, so
   nothing accumulates under a name a later pass reuses.
5. **`packages_digest` is computed over the stripped entries**, which is what
   removes the oracle: there is no longer a credential inside the pre-image. The
   alternative, keying the hash, needs a key source, a rotation story and a
   migration for digests already written; stripping needs none, because a
   credential-free entry strips to itself. One consequence, once: a list that
   carries a credential digests differently than it did before this change, so
   its first pass in an existing sandbox installs again.
6. **A credential a netrc cannot carry is left inline.** The decoded userinfo of
   a URL can contain a newline (percent-encoded in the URL, decoded before use),
   and no netrc quoting represents it. Such an entry keeps the credential it has
   today — the exposure is unchanged rather than newly created — and the
   executor logs that it did, at warn, naming the manager and the host but never
   the secret. Refusing the install instead would break a list that works today,
   for a shape that is exotic.

## What this does not close, stated rather than implied

**A same-sandbox read is not closed, and cannot be by this change.** The install
requires root (plan 40's decision 7 probes for it and refuses a non-root
sandbox), and the agent's own tool calls run in that same sandbox as the same
user — so a file the install reads is exactly as readable as the argv it
replaces, for the same window, by the same reader. The sandbox is one trust
domain and this change does not make it two.

What it does close is everything **outside** that domain: the credential no
longer reaches the Kubernetes apiserver's audit log, where the exposure is to
cluster operators and outlives the session; and `packages_digest` stops being an
offline oracle for a weak credential, for anyone holding an environment key.
`docs/self-hosted-security.md`'s paragraph on this says exactly that, in place of
the interim one that names #599 as the fix that has not landed.

## Acceptance

Each rung is a test that fails before the change and passes after.

1. **Extraction**, over the URL shapes that occur: `git+https://u:p@h/r`,
   `https://u:p@h/x.tgz`, a URL with no userinfo, a bare `user@` with no
   password, a percent-encoded credential (which must reach the file decoded), a
   non-URL entry, and an entry whose decoded credential carries a newline.
2. **The assembled command carries no credential.** Asserted on the exact
   command string for a credentialed list — not a substring probe that a
   rewording would stop exercising.
3. **The materialized files**, per transport: the netrc's quoting and escaping,
   the npmrc's base64 `_password` and its per-host keys, and that the npmrc is
   written for npm alone.
4. **The digest is over the stripped form**, and equals the digest of the same
   list written without its credential.
5. **The scratch directory is removed** when the install succeeds and when it
   fails.
6. **Mutation testing**, per the repo rule: every guard above gets a mutant that
   removes it, and each must die by a *named* test — not by a build failure and
   not by a hang.

## Docs

- `changelog.d/` — one fragment, `.security.md`: the argv and audit-log exposure
  and the digest oracle are both security-shaped.
- `docs/self-hosted-security.md` — the paragraph that describes the exposure and
  defers to #599 becomes what shipped, including the same-sandbox half it does
  not close.
- This file's status. **Not STATE.md**: it tracks active work, and plan 41 is
  the incumbent there; this lands whole in one PR, so its record is this file's
  frontmatter and docs/HISTORY.md.
