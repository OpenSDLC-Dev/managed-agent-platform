"""Read the corpus manifest, and refuse a corpus that nothing would check.

Two Python consumers read it — oracle.py beside this file and
deploy/gcp/check_split_test.py's corpus group — and the row rule has to be ONE
rule. It had already drifted between them before the first follow-up PR landed:
an empty `refuse` string reddened the merge gate and passed the Python group,
where `"" in err` then matched any refusal at all, and a row omitting `fmt` was
invisible to both and reached the oracle as a KeyError.
tools/kmsrole/corpus_test.go is the one mirror that genuinely cannot share this,
and it states the same rules in Go beside the same argument.

The rule is over VALUES and not over which keys are present, because Go's
decoder cannot tell an absent key from a JSON null or an empty string, and a
rule two languages state differently is the thing this file exists to prevent.
So a row is a blocks row when `blocks` holds a list, and a refusal row when
`refuse` holds a non-empty string; a stray `"blocks": null` beside a refusal is
ignored by both rather than refused by one.
"""

import json
import pathlib

HERE = pathlib.Path(__file__).parent


def row_problem(c):
    """What is wrong with one manifest row, or None."""
    if not isinstance(c.get("file"), str) or not c["file"]:
        return "a row names no file"
    if type(c.get("fmt")) is not int:
        return ("%s: no `fmt` exit recorded — `make tf-corpus-check` is what "
                "puts it to the binary" % c["file"])
    if bool(c.get("refuse")) == isinstance(c.get("blocks"), list):
        return ("%s: a row names either the blocks a reader returns or the "
                "refusal it gives, never both and never neither" % c["file"])
    return None


def load(root=HERE):
    """Return (cases, problems) for the corpus rooted at root.

    A non-empty problems list means the corpus is not checkable, which is a
    failure and not a pass: reading nothing must never read as clean.
    """
    cases = json.loads((root / "manifest.json").read_text())["cases"]
    problems = []
    if not cases:
        problems.append("the corpus manifest lists no cases")
    listed = [c.get("file") for c in cases]
    on_disk = {p.name for p in (root / "cases").glob("*.tf")}
    for name in sorted(on_disk - set(listed)):
        problems.append("%s is in the corpus and no manifest row names it" % name)
    for name in sorted(n for n in set(listed) - on_disk if n):
        problems.append("%s is named by a manifest row and is not in cases/" % name)
    if len(set(listed)) != len(listed):
        # Two rows naming one file leave a third unchecked with every name
        # still listed, which neither loop above can see.
        problems.append("%d manifest rows name only %d distinct files"
                        % (len(listed), len(set(listed))))
    problems.extend(p for p in (row_problem(c) for c in cases) if p)
    return cases, problems
