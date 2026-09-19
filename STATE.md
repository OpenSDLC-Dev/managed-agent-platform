# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#762 — nothing executable keeps the two `.tf` readers in agreement.**
`deploy/gcp/check_split.py` and `tools/kmsrole/hcl.go` are hand-written mirrors of one
reader, kept in agreement by prose: three commits on #760 read the same heredoc rule
three different ways, and every divergence was caught by a reviewer running terraform
rather than by a test. Three PRs — the shared corpus first, then the line rules folded
in from #766 and #768, and the interpolation context folded in from #767 last, which
needs machinery neither of the others did.
Everything else is closed: the archive-endings cluster shipped in v0.4.0
([docs/changelog/0.4.0.md](./docs/changelog/0.4.0.md)), and #720, #748, #749, #752,
#755, #750, #761, #765, #754 and #763 are unreleased
[changelog.d/](./changelog.d/) fragments.
The backlog is [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues).

## Tasks

- [x] `tools/tfcorpus` — one corpus, both suites reading it, terraform asked for each `fmt` exit
- [x] #768 — `\v`, `\f`, U+0085, U+2028 and U+2029 refused where they reach structure, including inside a `${…}` or `%{…}`, and read where terraform reads them
- [x] #766 — a heredoc opener carrying anything after the marker: refuse, rather than read the configuration behind it as string content
- [x] a heredoc tag Terraform takes and neither reader's class does (`<<Ö`) — found in review of the above, closed with it
- [x] #762's own second finding — the `SEPARATORS` subtraction stays, and docs/HISTORY.md records the spelled-out set as the rejected alternative
- [x] `/* */` inside `${…}` and `%{…}` is a comment to Terraform and now to both readers; a `#`/`//` there is refused, the readers having read on over a file Terraform rejects
- [ ] the rest of the interpolation context folded in from #767 (closed; #762 tracks it): a heredoc terminator closes only at depth 0
