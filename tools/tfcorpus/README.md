# The shared `.tf` reader corpus

`deploy/gcp/check_split.py` and `tools/kmsrole/hcl.go` are hand-written mirrors of
the same `.tf` reader in two languages. They cannot share code, and until #762
nothing executable tied one to the other: three consecutive commits on #760 read
the same heredoc rule three different ways, and every divergence was caught by a
reviewer running the terraform binary rather than by a test.

This directory is the tie. Each file under `cases/` is a whole `.tf` file; each
row of `manifest.json` says what a reader must do with it. Both suites read the
same manifest — `tools/kmsrole/corpus_test.go` in the merge gate, and
`check_split_test.py`'s corpus group under `make gcp-split-check-test` — so a
rule that moves in one reader and not the other fails on the side that did not
move.

## A row

```json
{
  "file": "heredoc_pad_leading.tf",
  "why": "an indented terminator closes a plain <<EOT, so the resource behind it is read",
  "fmt": 0,
  "blocks": ["google_kms_crypto_key.after"]
}
```

`blocks` is every top-level block a reader must return, in order, spelled the way
Terraform addresses it (`module.<label>` for a module). `refuse` replaces it
where the reader must refuse instead, and holds a substring **both** readers'
messages contain — the two wordings differ in punctuation and in what each
reader calls itself, so a row that quotes either one in full pins only that one.

Exactly one of the two per row, and `fmt` and `why` on every row — a row nobody
can review is a row nobody can tell from a row that pins nothing, and `why` is
what a rung reads aloud when a reader disagrees with the row. `loader.py` is
that rule in Python and `corpus_test.go` states it again in Go, because the Go
decoder cannot be taught Python's. It is a rule over **values**, not over which keys are
present: Go cannot tell an absent key from a JSON `null` or an empty string, so
a row is a blocks row when `blocks` holds a list — `[]` included, meaning a file
read cleanly with no top-level blocks in it — and a refusal row when `refuse`
holds a non-empty string.

## What the terraform rung does and does not check

`fmt` is the exit `terraform fmt -check` gives the file: `0` formatted, `3`
formatting drift, `1` or `2` a parse error. `make tf-corpus-check` runs the
binary over every case and compares, so a terraform upgrade that moves one is
read there rather than discovered by the next reviewer. That is the property
#758 and #761 both turned on — each was a rule the two readers agreed on and
terraform disagreed with.

It stops there. `fmt` says whether the file parses; it never says **where**
terraform closed a heredoc, so a row's `blocks` and `refuse` stay human-derived
and what holds them is the two readers having to agree. A shared misreading that
still parses is the gap this rung cannot close.

## Adding a row

Write the `.tf` file under `cases/`, run `make tf-corpus-check` to learn its
`fmt` exit, and add the row. Every rung fails on a `.tf` no row names and on a
row naming no file, so a fixture cannot be added and left unchecked.

Before you trust a new row, break the rule it claims to pin and check the
**answer changes**. Rows here have failed that four ways, each found by someone
running a mutation rather than by reading: a hidden resource inside a `locals`
block, where a wrong heredoc close never lets the brace depth return to zero; a
shielded region with nothing structural in it at all; a balanced brace where the
point was the neutralisation; and a file refused for an unrelated reason, which
pins that reason instead. An unbalanced `{` in the shielded region is usually
what fixes the first three, and the fourth needs a different file.

A row naming several characters has to CONTAIN several characters, in each
position it names. A row that said "these five" and held three read as coverage
and was a sample: a mutant dropping either of the missing two left the whole
corpus green.

Where a rule is defended twice over, one row cannot show the second defence
failing, and its `why` has to say which half it holds and which row holds the
other — `utf8_backslash_before_runes.tf` and `utf8_bad_escaped_in_string.tf` are
that pair. A row pinning a combination is fine; a row whose `why` reads as
though it pinned a single rule it does not is the defect.

## Two things worth knowing

**A file terraform accepts that a reader refuses is not a defect by itself.**
Reading a quoted string inside `${…}` or `%{…}`, or a string still open at end
of line, needs real HCL lexing, and both readers refuse deliberately rather than
desync their brace depth around an unread resource. Such a row has to carry
`"terraform_accepts": true`, and `make tf-corpus-check` fails both ways — a
deliberate refusal that does not say so, and a row that says so without being
one — so the standing set cannot grow without a row admitting to it.

**This lives in `tools/` and not under `deploy/gcp/`** because
`tools/kmsrole`'s tests run inside `make verify`, and the merge gate must not
depend on `deploy/gcp` (plan 20, Decision 9). The corpus belongs to neither
reader, which is also why it is not `tools/kmsrole/testdata`.

## The alternative it beat

A sidecar per case — `cases/x.tf` beside `cases/x.json` — would make the pairing
structural and delete the reconciliation every rung now performs. It was not
taken because one manifest answers "what does the corpus pin?" by being read
once, while sidecars answer it only by being globbed, and because the
reconciliation is two implementations (this directory's `loader.py`, and Go's
mirror) rather than three. Revisit it if a third language ever needs the corpus.
