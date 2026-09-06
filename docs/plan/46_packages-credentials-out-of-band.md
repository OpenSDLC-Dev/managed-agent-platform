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
| `npm` 11's own fetcher (a tarball URL) | `$HOME/.npmrc`, per-host `username` + `_password` (base64) | `Authorization: Basic`, on the **first** request |
| `npm` 6's own fetcher (the same URL) | the same file | **nothing** — with `always-auth` and without it |

Three properties of the netrc format were measured with them, because each
decides what the writer may emit. A `machine` line matches on the **hostname
alone**, port excluded, and case-insensitively. A value **may** be double-quoted
— curl reads one, `\"` and `\\` escapes included. And the writer nevertheless
**must not** quote, because curl is not the only reader: pip's own fetcher goes
through Python's `netrc` module, which did not strip quotes before 3.11 —
measured, python 3.10.21 returns `"bot"` where 3.12.14 returns `bot`, and
Ubuntu 22.04 and Debian bullseye ship that Python. A quoted file would make pip
send the quotes and the origin refuse. So values are written bare, and a
credential a bare value cannot carry keeps its entry.

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

1. **The scan looks for a URL credential anywhere in the entry, not for an entry
   that is a URL.** The entry is often not the URL: pip's PEP 508 direct
   reference (`private-lib @ git+https://user:token@host/repo`) and npm's alias
   (`private-lib@https://user:token@host/pkg.tgz`) each nest one, and both are
   ordinary syntax their managers accept. So the test is a regexp of the shape
   `internal/executor`'s own message redactor already uses for the same job on
   the way out — a scheme, a userinfo that runs to the last `@` before the
   authority ends, and a host — and the span it finds is re-read by `url.Parse`,
   which decodes the percent-encoding and splits an IPv6 literal from its port.
   The scan locates; the parser interprets; the cut is textual, so no other byte
   of the entry moves. A `user@host` with no password is a name and is left
   alone. What this still does not see is a credential in a **query parameter**
   (`?token=…`), which no rule can tell from an ordinary parameter — the
   redactor covers it on output, and nothing here claims to.
2. **Only where the fetcher reads what this writes — which is a question about
   the manager as well as the scheme.** The scheme must be `http`, `https`,
   `git+http` or `git+https`: `git+ssh` never used the URL's password (ssh takes
   none from a URL), and `hg+https`, `svn+https` and `bzr+http` authenticate from
   their own stores. And the *manager* must be one whose fetch reads a netrc at
   all — pip's own client and git do, which covers pip, npm and go; `apt`,
   `cargo` and `gem` read `auth.conf.d`, `credentials.toml` and
   `~/.gem/credentials`, so moving their credential into a netrc would break an
   install that works today rather than protect one. Everything else keeps its
   credential where it is.
3. **One hostname cannot hold two credentials.** A netrc `machine` line matches
   on the hostname alone, port and path excluded, so two entries naming one host
   with different credentials have no representation that keeps them apart:
   whichever was written would be sent to both, and one service would receive
   the other's secret. That is worse than the argv exposure it replaces, so a
   host two entries disagree about keeps both of them inline. Two details decide
   what counts: the host is folded first — matching is case-insensitive and a
   trailing dot names the same host — so `Registry.Example` and
   `registry.example.` cannot slip past as two hosts; and what is compared is the
   **login**, not the whole credential, so one host reached on two ports with the
   same user and secret is one netrc line and two npmrc keys, not a
   disagreement. A URL naming the host with **no** credential at all is a
   disagreement too, and the sharpest one: the line would be sent there
   unasked — pip sends Basic from a netrc on the *first* request, not on a 401 —
   so a secret meant for `registry.example:8443` would start arriving at
   `registry.example:443`, or over plain `http`, at a service that never
   received it. A URL carrying its own credential is unaffected, because its own
   wins; that is why an absent one, and only an absent one, has to count. This
   is why the scan reads every URL in the list rather than only the ones with a
   userinfo.
4. **The credential is materialized into a scratch `HOME`, and the install for
   that manager runs with `HOME` pointed at it.** `$HOME/.netrc` always;
   `$HOME/.npmrc` additionally for npm, whose fetcher reads no netrc. Both are
   written `0600` rather than the `0644` a zero mode lands: it does not close
   the same-user read below, since the install and the agent share a root, but
   an image carrying any other user no longer has them readable by default. The
   alternative — writing into the image's own `/root` and restoring afterwards —
   needs a read-modify-write and a restore path that can leave a credential
   behind when it fails. A scratch directory has neither. The cost is stated
   rather than hidden: for that one install, an image that ships its own
   `~/.npmrc` or `~/.netrc` is not read, and the manager's `HOME`-rooted cache
   is cold. Both apply only to a manager whose list actually carries a
   credential. **No `always-auth`** goes in the npmrc: npm 11 authenticates from
   the per-host pair alone and warns that the key is unknown, and npm 6 sends no
   credential for a non-registry fetch with or without it (both measured) — so
   an image shipping npm 6 is the one shape this pass makes worse, and the
   security guide says so.
5. **The directory name is random.** The sandbox is agent-writable, so a fixed
   path could be pre-created as a regular file to make the write fail; a random
   one cannot be waited for.
6. **The removal is asked for twice, because once is not enough.** The install
   command carries a `trap … EXIT` set before its preflight, which removes the
   directory the moment the group ends of its own accord — including the
   preflight's 127. Two things were measured about that trap and both changed
   the code: a subshell whose last command is the install is *replaced* by it on
   bash 3.2, taking the EXIT trap with it (bash 5 keeps it), so the group ends
   on `exit "$?"`, a builtin, carrying the status the classification reads; and
   a **timed-out** install is killed with SIGKILL to its process group on both
   backends, which no trap survives at all. So `installPackages` asks for the
   directory again after the install returns — also when a write or the exec
   itself failed first. That second removal carries **its own** short budget and
   is preceded by a progress tick: two calls each carrying the install timeout
   would put twice the stall floor's longest single step inside one silent
   interval, which is the reclaim loop #383 is about. What neither removal
   survives is this executor dying in between; the directory then lives as long
   as the sandbox, and the next pass writes to a fresh random name rather than
   that one.
7. **Two digests, because they answer different questions.** The **sentinel**
   inside the sandbox keeps comparing the list as written, credential included:
   a rotated credential has to read as a changed list, or a corrected credential
   inherits the exhausted attempt count of the broken one and never installs.
   Plan 40 already put that digest there, so nothing is newly exposed. The
   **published** `packages_digest`, which rides on an event an environment key
   can read while the config it digests needs a management key, is taken over
   the stripped form — that is what stops it being an offline oracle for a weak
   credential. Keying the hash instead needs a key source, a rotation story and
   a migration for digests already written; stripping needs none. One
   consequence has to be paid for rather than admired: the event dedupe keys on
   the published digest, which a rotation no longer changes, so a corrected
   credential's failures would have been suppressed as repeats of the broken
   one's and a client would have seen silence. The caller knows the list changed
   — the sentinel is what tells it — and says so, and a changed list is emitted
   without consulting the history. "Changed" means the sentinel holds a record
   whose digest differs, and deliberately not "the sentinel holds no record":
   the refusal paths below emit and return without writing one, so a stored
   invalid list has no record on any pass, and reading that as a change would
   skip the dedupe every time and append the same exhausted error on every tool
   call for the life of the sandbox. With no record there is nothing to have
   changed from, and the query is the right answer.
8. **A credential a bare netrc line cannot carry is left inline.** Values are
   written unquoted (the measurement above says why), so a decoded userinfo
   carrying whitespace, a `"`, a `\`, a `#` or a control character has no
   representation here — a URL can carry every one of them percent-encoded, and
   they decode before use. So can a hostname the parser reads as empty, and a
   userinfo Go's URL grammar refuses where a manager would not (an unescaped
   `^`, a malformed `%` escape). Each such entry keeps the credential it has
   today — the exposure is unchanged rather than newly created — and the
   executor logs at warn that it did, naming the manager and the host but never
   the secret. The same line covers decisions 2 and 3's leftovers. Refusing the
   install instead would break a list that works today.

## What this does not close, stated rather than implied

**A same-sandbox read is not closed, and cannot be by this change.** The install
requires root (plan 40's decision 7 probes for it and refuses a non-root
sandbox), and the agent's own tool calls run in that same sandbox as the same
user — so a file the install reads is exactly as readable as the argv it
replaces, for the length of the install. The sandbox is one trust domain and
this change does not make it two.

**Six entry shapes keep the credential they have today**, each for a reason the
decisions argue: a credential in a query parameter, which nothing can tell from
an ordinary parameter; a transport that reads neither file; a *manager* that
reads neither file (`apt`, `cargo`, `gem`); a hostname two entries disagree
about — including a host some other URL in the same list names with no
credential at all; a value a bare netrc cannot carry; and a userinfo Go's URL
grammar refuses where a manager would not. For those the argv and audit-log exposure is
exactly what it was — unchanged, not newly created — **and so is the digest**,
since the published one is taken over whatever survived the strip. All but the
first are named in a warn line rather than left silent.

**The scratch `HOME` hides more than the two files it holds.** Everything rooted
at `HOME` goes with it for that one install — `~/.config/pip/pip.conf` and
`$CARGO_HOME` included — so an image that bakes a private index into pip.conf
loses it for the install that carries a credential, and every `HOME`-rooted
cache is cold. Only that install, and only a manager whose list carries a
credential.

**An image shipping npm 6 loses an install it had.** npm 6 sends no credential
for a non-registry fetch from any `.npmrc` key (measured), so an npm tarball URL
whose credential this pass lifts out authenticates with nothing and fails 401
where it used to succeed. npm 7 and later are unaffected. That is the one place
this trades a working install for the audit-log exposure, and the security guide
says so rather than leaving it to be discovered.

What it does close is everything **outside** the sandbox for every other shape:
the credential no longer reaches the Kubernetes apiserver's audit log, where the
exposure is to cluster operators and outlives the session; and the published
`packages_digest` stops being an offline oracle for a weak credential, for
anyone holding an environment key.

## Acceptance

Each rung is a test that fails before the change and passes after.

1. **Extraction**, over the shapes that occur: a whole-entry `git+https://u:p@h/r`,
   a tarball URL with a port, pip's PEP 508 nesting, npm's alias nesting, a URL
   with no userinfo, a bare `user@` with no password, a percent-encoded
   credential (which must reach the file decoded), a non-URL entry, an `@` in a
   path, a scheme whose fetcher reads nothing this writes, and an entry whose
   decoded credential carries a newline.
2. **The assembled command carries no credential.** Asserted on the exact
   command string for a credentialed list — not a substring probe that a
   rewording would stop exercising.
3. **The materialized files**, per transport: the netrc's bare, unquoted values
   and its one line per host, the npmrc's base64 `_password` and its per-host
   keys, that the npmrc is written for npm alone, and that both land `0600`.
4. **One hostname the list disagrees about** leaves every entry on it alone and
   writes no file — two different credentials, and a second URL naming the host
   with none, on another port and over plain `http` alike; the same credential
   twice is written once, and an uncredentialed URL for a *different* host is
   not a disagreement.
5. **The digests**: the published one equals the digest of the same list written
   without its credential, and the sentinel's does not — a rotated credential is
   a changed list. And a list refused before any sentinel is written is one
   event however many tool calls follow it.
6. **The removal**, driven through a real shell rather than asserted as a
   substring: the trap removes the directory when the install fails and when the
   preflight refuses, the status the classification reads survives, and the
   executor asks for the directory again after the install returns.
7. **Mutation testing**, per the repo rule: every guard above gets a mutant that
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
