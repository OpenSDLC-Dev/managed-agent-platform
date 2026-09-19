# STATE.md — Active work

What is being worked on right now, and how far along it is — nothing else. **Size budget: ~30 lines.** Everything static lives elsewhere: conventions and the doc index in [CLAUDE.md](./CLAUDE.md), the as-built system in [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md), a change's narrative (written once) as a [changelog.d/](./changelog.d/) fragment assembled into [CHANGELOG.md](./CHANGELOG.md) at release time, the backlog in GitHub issues. The verifier checks this file's claims against reality on its docs-consistency rung.

## Active work

**#762 — nothing executable keeps the two `.tf` readers in agreement.**
`deploy/gcp/check_split.py` and `tools/kmsrole/hcl.go` are hand-written mirrors of one
reader, kept in agreement by prose: three commits on #760 read the same heredoc rule
three different ways, and every divergence was caught by a reviewer running terraform
rather than by a test. Two PRs — the shared corpus first, then the three reader
boundaries folded in from #766, #767 and #768.
Everything else is closed: the archive-endings cluster shipped in v0.4.0
([docs/changelog/0.4.0.md](./docs/changelog/0.4.0.md)), and #720, #748, #749, #752,
#755, #750, #761, #765, #754 and #763 are unreleased
[changelog.d/](./changelog.d/) fragments.
The backlog is [GitHub issues](https://github.com/OpenSDLC-Dev/managed-agent-platform/issues).

## Tasks

- [x] `tools/tfcorpus` — one corpus, both suites reading it, terraform asked for each `fmt` exit
- [ ] #766 — a heredoc opener carrying a trailing comment: refuse, rather than read the configuration behind it as string content
- [ ] #767 — interpolation context: a terminator closes only at interpolation depth 0, and `#`/`//`/`/* */` inside `${…}` are comments
- [ ] #768 — `\v`, `\f`, U+2028 and U+0085 refused where they reach structure, read where terraform reads them
- [ ] #762's own second finding — the CPython-`\s` vs Terraform-whitespace spelling behind `SEPARATORS`; the `#767` citations in both readers are repointed with it
