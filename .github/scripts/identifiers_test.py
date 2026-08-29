#!/usr/bin/env python3
"""Fail if an operator's real coordinates are written into the documentation.

#355/#356 parameterised deployment identifiers out of the repository by hand.
The sweep reached `deploy/` and `.github/` and stopped there, and nothing in the
repository could notice, so what it left sat in a public repo for months: #514
found a project id, two project numbers, an Artifact Registry path and two
routable LoadBalancer addresses still in `docs/HISTORY.md`. A rule enforced only
by memory is a rule that lapses. This is the enforcing half of that fix.

A guard cannot search for the values themselves -- it would have to contain
them, which is the thing being prevented -- so it searches for shapes:

  - a ROUTABLE IPv4 address. Every address these documents legitimately use is
    loopback, RFC 1918, link-local, multicast, or an RFC 5737 documentation
    range. A routable one in a deployment record is somebody's live endpoint.
  - a BARE 12-DIGIT INTEGER, the shape of a GCP project number. The lookarounds
    are load-bearing: skill version ids in these documents are 16 digits, and a
    substring match flags every one of them.
  - an ARTIFACT REGISTRY PATH or `*.iam.gserviceaccount.com` ADDRESS whose
    project component is not one of this repository's placeholders. This is the
    shape that leaked a project id in #514, and every legitimate instance today
    already uses a placeholder, so the rule costs nothing and holds the
    convention as well. A real project NUMBER in a `cloudbuild`/`developer`
    service account needs no rule of its own -- it is twelve digits.

WHAT IT DOES NOT COVER, stated plainly because the surrounding prose must not
claim more than this. A bare project id in running text has no shape to match --
it is a lowercase-hyphen word like any other -- so it is caught only where it
appears inside one of the structured forms above. IPv6 endpoints, bucket names
and KMS key names have no rule. This catches four shapes; it is not a proof that
the repository is clean.

A KNOWN FALSE POSITIVE, recorded rather than worked around. The registry and
service-account rule flags any project component that is not a placeholder, so a
genuinely public third-party project -- a Google sample image path, say -- goes
red. That is one entry in PLACEHOLDER_PROJECTS when it first happens, and it is
left as a loud, obvious failure rather than pre-empted with a list of names
nobody would maintain. A four-component version string, and a 12-digit timestamp,
are flagged for the same kind of reason and take the same kind of remedy.

Markdown only, and that scope was measured rather than assumed: widening from
`docs/` and `deploy/` to every tracked `*.md` produced not one new finding -- the
same four, all in one file -- so the scope is all of them, which is what puts
CHANGELOG.md and the `changelog.d/` fragments that become it inside the guard.
The count the run prints is the live number; this paragraph deliberately quotes
none, because a number in prose goes stale the next time a document is added.
Source files are
outside it, and that is a real gap rather than an oversight: #514 found a project
id in a Go test fixture too, but every networking test here is full of addresses
on purpose, and a guard that cried wolf there would be switched off within a
week. Prose is where a deployment record gets written down; code is where
addresses are the subject matter.

This file is not scanned either, which is only safe because every fixture below
is synthetic -- `11.22.33.44` and `123456789012` are nobody's coordinates. An
earlier draft used the real values this change removes, which republished them in
the one file the guard cannot see. Keep the fixtures synthetic.

The self-test runs FIRST, before any repository access. Without it a broken
pattern reports exactly what a clean repository reports, and the silence is
indistinguishable.
"""

import ipaddress
import re
import subprocess
import sys
from pathlib import Path

# Placeholders the documentation already uses where a project belongs. A value
# outside this set is somebody's real project, which is the whole finding.
PLACEHOLDER_PROJECTS = frozenset({
    "your-project", "my-project", "example-project",
    "PROJECT", "PROJECT_ID", "PROJECT_NUMBER", "P", "p",
})

# Allowed by value: famous third-party resolvers, used as examples of a service,
# never as anybody's coordinate.
ALLOWED_ADDRESSES = frozenset({"8.8.8.8", "1.1.1.1"})

# Documents the scan must always reach. Losing the scope entirely already fails,
# but narrowing it — a mistyped pathspec, an exclusion that catches too much —
# would leave a shorter list and a smaller printed count that nothing checks. If
# one of these is ever renamed, this fails loudly and the list gets edited, which
# is the correct amount of friction for a file that is meant to always be scanned.
REQUIRED_DOCUMENTS = ("CLAUDE.md", "STATE.md", "docs/HISTORY.md")

# The trailing lookahead rejects only a dot that CONTINUES the number. Rejecting
# any dot would miss an address at the end of a sentence, which is the commonest
# way prose writes one down.
IPV4 = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9])(?!\.[0-9])")
PROJECT_NUMBER = re.compile(r"(?<![0-9])[0-9]{12}(?![0-9])")
REGISTRY = re.compile(r"[a-z0-9-]+-docker\.pkg\.dev/([A-Za-z0-9_-]+)")
SERVICE_ACCOUNT = re.compile(r"@([A-Za-z0-9-]+)\.iam\.gserviceaccount\.com")


def routable(text):
    """The address in `text`, if it is one and it is somebody's real endpoint."""
    try:
        ip = ipaddress.ip_address(text)
    except ValueError:
        return False  # 999.999.999.999 and friends are not addresses at all
    if text in ALLOWED_ADDRESSES:
        return False
    if ip.is_multicast:
        return False  # 224.0.0.251 and kin answer is_global True but address nobody
    # `is_global` already excludes loopback, RFC 1918, link-local and the RFC 5737
    # documentation ranges — measured, not assumed: an explicit exclusion for the
    # documentation ranges was written first and removing it changed nothing. The
    # TEST-NET rows in the self-test are what hold that, so a runtime that ever
    # disagrees goes red here rather than quietly flagging every example address.
    return ip.is_global


def violations(line):
    """Every reason this one line should not be in the repository."""
    found = []
    for m in IPV4.finditer(line):
        if routable(m.group(0)):
            found.append(f"routable address {m.group(0)}")
    for m in PROJECT_NUMBER.finditer(line):
        found.append(f"project number {m.group(0)}")
    for label, pattern in (("registry path", REGISTRY),
                           ("service account", SERVICE_ACCOUNT)):
        for m in pattern.finditer(line):
            project = m.group(1)
            if project in PLACEHOLDER_PROJECTS:
                continue
            # `gcp-sa-*` is Google's own reserved namespace for the service agents
            # it creates in your project (service-N@gcp-sa-cloudbuild…). The label
            # is Google's, never an operator's project id, so it can no more be a
            # coordinate than a multicast address can be an endpoint.
            if project.startswith("gcp-sa-"):
                continue
            found.append(f"{label} naming project {project!r}")
    return found


def scan_texts(documents):
    """The scanning half, separated so the self-test can drive it without git."""
    bad = []
    for rel, text in documents:
        for n, line in enumerate(text.splitlines(), 1):
            for why in violations(line):
                bad.append(f"{rel}:{n}: {why}")
    return bad


def tracked_docs(root):
    """Every tracked markdown path that is documentation.

    `-z` rather than newlines: a tracked path containing a space would otherwise
    be split into two nonexistent paths and silently skipped, which is the
    failure mode this whole file exists to prevent. `testdata` is compared as a
    path component so a top-level `testdata/` is excluded too.
    """
    out = subprocess.run(
        ["git", "ls-files", "-z", "--", "*.md"],
        cwd=root, capture_output=True, text=True, check=True,
    ).stdout
    paths = [p for p in out.split("\0") if p]
    return sorted(p for p in paths if "testdata" not in Path(p).parts)


def read_documents(root, paths):
    """(path, text) for each, or a hard error. Never a silent skip.

    A file that cannot be read or decoded is reported as a failure rather than
    passed over: a guard that reports clean on the documents it could not open is
    worse than no guard, because it is believed.
    """
    documents, unreadable = [], []
    for rel in paths:
        try:
            documents.append((rel, (root / rel).read_text(encoding="utf-8")))
        except (OSError, UnicodeDecodeError) as e:
            unreadable.append(f"{rel}: {type(e).__name__} — not scanned")
    return documents, unreadable


def selftest():
    """Prove each rule fires and each exemption holds. Rows, not prose.

    Every value here is invented. See the module docstring: an earlier draft used
    the real coordinates this change removes, in the one file the guard does not
    scan.
    """
    must_flag = [
        ("a routable address", "LoadBalancer `11.22.33.44:8080`"),
        ("a routable address ending a sentence", "The endpoint was 11.22.33.44."),
        ("a routable address in a list", "reached 11.22.33.44, then stopped"),
        ("a project number", "project (123456789012) and"),
        ("a project number, line-initial", "123456789012 was the number"),
        ("a registry path naming a project",
         "into us-central1-docker.pkg.dev/a-real-project/map-images"),
        ("a service account naming a project",
         "as map-brain@a-real-project.iam.gserviceaccount.com"),
    ]
    must_pass = [
        ("loopback", "listens on 127.0.0.1:8080"),
        ("RFC 1918", "the private IP 10.1.2.3"),
        ("RFC 1918 172.16/12", "peered at 172.16.0.4"),
        ("RFC 1918 192.168/16", "getent returns 192.168.65.254"),
        ("link-local", "metadata at 169.254.169.254"),
        ("unspecified", "binds 0.0.0.0:8080"),
        ("multicast", "mDNS at 224.0.0.251"),
        ("RFC 5737 TEST-NET-1", "example 192.0.2.1"),
        ("RFC 5737 TEST-NET-2", "example 198.51.100.7"),
        ("RFC 5737 TEST-NET-3", "example 203.0.113.5"),
        ("allowed public resolver", "resolves via 8.8.8.8 and 1.1.1.1"),
        ("not an address at all", "the invalid 999.999.999.999"),
        ("a 16-digit skill version", "version=1784657206256533 dest=x"),
        ("an 11-digit number", "run 31260884425 was green"),
        # Three components, so IPV4 never reaches routable(). It guards the other
        # direction: widening the pattern to three would light up every version
        # string in these documents. A FOUR-component version is genuinely
        # indistinguishable from an address and will be flagged; the allowlist is
        # the remedy if one is ever written down.
        ("a three-part version", "the real `ant` CLI 1.21.0 built"),
        ("a placeholder registry path",
         "e.g. us-central1-docker.pkg.dev/your-project/map-images"),
        ("a placeholder service account",
         "map-controlplane@your-project.iam.gserviceaccount.com"),
        ("a Google-managed service agent",
         "granted service-9@gcp-sa-cloudbuild.iam.gserviceaccount.com"),
    ]
    failures = []
    for label, line in must_flag:
        if not violations(line):
            failures.append(f"MISSED   {label}: {line!r}")
    for label, line in must_pass:
        got = violations(line)
        if got:
            failures.append(f"FALSE +  {label}: {line!r} -> {got}")

    # scan_texts itself, so that a scanner which always returns [] cannot pass
    # while every row above stays green.
    planted = scan_texts([("a.md", "fine\nLoadBalancer 11.22.33.44 here\n")])
    if planted != ["a.md:2: routable address 11.22.33.44"]:
        failures.append(f"SCAN     did not report the planted line: {planted}")
    return failures


def main():
    failures = selftest()
    if failures:
        print("the detector itself is broken, so a clean scan would mean nothing:")
        for f in failures:
            print(f"  {f}")
        return 1
    print("detector self-test: ok")

    root = Path(subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True, text=True, check=True,
    ).stdout.strip())

    paths = tracked_docs(root)
    if not paths:
        print("no tracked markdown found — the scan would be clean for the wrong reason")
        return 1
    missing = [d for d in REQUIRED_DOCUMENTS if d not in paths]
    if missing:
        print("the scan did not reach documents it must always cover, so a clean "
              "result would mean nothing:")
        for m in missing:
            print(f"  {m}")
        return 1
    documents, unreadable = read_documents(root, paths)
    if unreadable:
        print("these documents could not be scanned, so this run proves nothing:")
        for u in unreadable:
            print(f"  {u}")
        return 1

    bad = scan_texts(documents)
    if bad:
        print("operator coordinates must be GitHub Actions variables, not repository "
              "content (#355, #356, #514):")
        for b in bad:
            print(f"  {b}")
        return 1
    print(f"ok — {len(documents)} documents carry none of the four shapes")
    return 0


if __name__ == "__main__":
    sys.exit(main())
