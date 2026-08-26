#!/usr/bin/env python3
"""Table test for parked.sh, the signal deploy.yml and staging-parked.yml share.

Run it rather than read it — `make parked-test`.

The whole risk in this file is one character. The rule is "a KEY beginning with
`power-saved-`", and the tempting spelling of it — a bare `*power-saved-*` — also
matches a label VALUE that happens to contain the prefix. Both directions are
silent and both are expensive: a false `parked` stops deploying to a cluster
nobody parked, and a false `live` leaves a forgotten environment billing with
nothing said. So the rows that matter here are not the obvious ones; they are the
near-misses below the fold.

The label strings are gcloud's `value(resourceLabels)` rendering, `k=v;k=v`.
`goog-terraform-provisioned` is real — the provider writes it, which is why
env-power.sh takes such care to preserve other people's labels — so the rows
carrying it are the shapes this actually meets in production.
"""

import pathlib
import subprocess
import sys

SCRIPT = pathlib.Path(__file__).with_name("parked.sh")

# (resource labels, verdict, what this stands for)
CASES = [
    # The two ordinary answers.
    ("", "live", "a cluster with no labels at all"),
    ("goog-terraform-provisioned=true", "live", "the label the provider writes, and nothing else"),
    ("power-saved-platform=2;power-saved-sandbox=2", "parked",
     "both pools parked, exactly as env-power.sh writes them"),
    # Position must not matter: env-power.sh writes everyone else's labels FIRST
    # and appends its own, so in practice the marker is never the first key.
    ("goog-terraform-provisioned=true;power-saved-platform=2;power-saved-sandbox=2", "parked",
     "the production shape — provider label first, ours appended"),
    ("power-saved-platform=2;goog-terraform-provisioned=true", "parked",
     "ours first, someone else's after"),
    ("power-saved-platform=2", "parked", "one pool parked, one added to main.tf later"),
    # The near-misses. Every one of these is `live`, and a bare substring match
    # calls the first two `parked`.
    ("deployment-note=power-saved-example", "live",
     "a VALUE containing the prefix — the trap the `;` anchor exists for"),
    ("env=power-saved-platform", "live", "a value that is exactly what a key would be"),
    ("x-power-saved-platform=2", "live", "a key CONTAINING the prefix but not beginning with it"),
    ("powersaved-platform=2", "live", "a key one hyphen away"),
    ("power_saved-platform=2", "live", "a key one underscore away"),
    ("power-saved=2", "live", "the prefix without its trailing hyphen is not the prefix"),
    # Ordinary labels that must stay quiet.
    ("team=platform;cost-center=eng", "live", "unrelated labels"),
    ("a=b;c=d;e=f", "live", "several unrelated labels"),
]


def run(argv):
    return subprocess.run(
        ["bash", str(SCRIPT), *argv],
        capture_output=True, text=True, check=False,
    )


def main():
    failures = []
    for labels, want, why in CASES:
        got = run([labels])
        actual = got.stdout.strip()
        if actual != want or got.returncode != 0:
            failures.append(
                f"  {labels!r:60s} -> {actual or '(nothing)'!r} (exit {got.returncode}),"
                f" want {want!r} — {why}"
            )

    # An empty label string is a REAL input — a cluster with no labels — so it
    # must be one argument and not zero. A caller that forgets to quote it turns
    # `live` into a usage error rather than into a silent wrong answer, which is
    # the whole point of checking arity here.
    for argv in ([], ["a=b", "c=d"]):
        got = run(argv)
        if got.returncode != 2:
            failures.append(f"  {argv!r} -> exit {got.returncode}, want 2 (usage)")

    if failures:
        print(f"parked.sh: {len(failures)} case(s) wrong:", file=sys.stderr)
        print("\n".join(failures), file=sys.stderr)
        return 1
    print(f"ok: parked.sh reads all {len(CASES)} label shapes, and refuses bad arity")
    return 0


if __name__ == "__main__":
    sys.exit(main())
