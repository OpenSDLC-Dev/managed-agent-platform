#!/usr/bin/env python3
"""Fail if an operator's real coordinates are written into the documentation.

#355/#356 parameterised deployment identifiers out of the repository by hand.
The sweep reached `deploy/` and `.github/` and stopped there, and nobody noticed
for months: #514 found a project id, two project numbers, an Artifact Registry
path and two routable LoadBalancer addresses still sitting in `docs/HISTORY.md`.
A rule enforced only by memory is a rule that lapses, so this is the enforcing
half of that fix.

WHAT IT LOOKS FOR, and why only these two shapes. A guard cannot search for the
values themselves -- it would have to contain them, which is the thing being
prevented. So it searches for two shapes that no documentation in this
repository has a legitimate reason to carry:

  - a ROUTABLE IPv4 address. Every address these documents legitimately use is
    loopback, RFC 1918, link-local, or an RFC 5737 documentation range. A public
    address in a deployment record is somebody's live endpoint.
  - a BARE 12-DIGIT INTEGER, which is the shape of a GCP project number. The
    lookarounds matter: skill version ids in these documents are 16 digits, and
    a substring match would flag every one of them.

Two public resolvers are allowed by value. `8.8.8.8` and `1.1.1.1` are used as
examples of a well-known third-party service, not as anybody's coordinates.

WHAT IT DELIBERATELY DOES NOT COVER. Markdown only. Widening from `docs/` and
`deploy/` to every tracked `*.md` was measured first and cost nothing -- not one
new finding across 113 files -- so the scope is all of them, which is what puts
CHANGELOG.md and the `changelog.d/` fragments that become it inside the guard.
Source files are outside it, and that is a real gap rather than an oversight:
#514 found a project id in a Go test fixture too, but every networking test in
this repository is full of addresses on purpose, and a guard that cried wolf
there would be switched off within a week. Prose is where a deployment record
gets written down; code is where addresses are the subject matter.

The self-test runs FIRST, on every invocation. Without it a broken pattern
reports exactly what a clean repository reports, and the silence is
indistinguishable.
"""

import ipaddress
import re
import subprocess
import sys
from pathlib import Path

ALLOWED_ADDRESSES = {"8.8.8.8", "1.1.1.1"}

IPV4 = re.compile(r"(?<![0-9.])(?:[0-9]{1,3}\.){3}[0-9]{1,3}(?![0-9.])")
PROJECT_NUMBER = re.compile(r"(?<![0-9])[0-9]{12}(?![0-9])")


def routable(text):
    """The address in `text`, if it is one and it is somebody's real endpoint."""
    try:
        ip = ipaddress.ip_address(text)
    except ValueError:
        return False  # 999.999.999.999 and friends are not addresses at all
    if text in ALLOWED_ADDRESSES:
        return False
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
    return found


def tracked_docs(root):
    """Every tracked markdown file that is documentation.

    `testdata/` is the only exclusion: those four files are fixtures for the
    skill-upload suite, not prose anyone reads as a record of a deployment.
    """
    out = subprocess.run(
        ["git", "ls-files", "--", "*.md"],
        cwd=root, capture_output=True, text=True, check=True,
    ).stdout.split()
    return sorted(p for p in out if "/testdata/" not in p)


def scan(root):
    bad = []
    for rel in tracked_docs(root):
        path = root / rel
        try:
            text = path.read_text(encoding="utf-8", errors="replace")
        except OSError:
            continue
        for n, line in enumerate(text.splitlines(), 1):
            for why in violations(line):
                bad.append(f"{rel}:{n}: {why}")
    return bad


def selftest():
    """Prove each rule fires and each exemption holds. Rows, not prose."""
    must_flag = [
        ("a routable address", "LoadBalancer `34.63.227.73:8080`"),
        ("a routable address, bare", "reached 203.0.114.9 directly"),
        ("a project number", "project (754963270337) and"),
        ("a project number, line-initial", "460647310105 was the number"),
    ]
    must_pass = [
        ("loopback", "listens on 127.0.0.1:8080"),
        ("RFC 1918", "the private IP 10.136.0.3"),
        ("RFC 1918 172.16/12", "peered at 172.16.0.4"),
        ("RFC 1918 192.168/16", "getent returns 192.168.65.254"),
        ("link-local", "metadata at 169.254.169.254"),
        ("unspecified", "binds 0.0.0.0:8080"),
        ("RFC 5737 TEST-NET-1", "example 192.0.2.1"),
        ("RFC 5737 TEST-NET-2", "example 198.51.100.7"),
        ("RFC 5737 TEST-NET-3", "example 203.0.113.5"),
        ("allowed public resolver", "resolves via 8.8.8.8 and 1.1.1.1"),
        ("not an address at all", "the invalid 999.999.999.999"),
        ("a 16-digit skill version", "version=1784657206256533 dest=x"),
        ("an 11-digit number", "run 31260884425 was green"),
        ("a dotted version", "the real `ant` CLI 1.21.0 built"),
    ]
    failures = []
    for label, line in must_flag:
        if not violations(line):
            failures.append(f"MISSED   {label}: {line!r}")
    for label, line in must_pass:
        got = violations(line)
        if got:
            failures.append(f"FALSE +  {label}: {line!r} -> {got}")
    return failures


def main():
    root = Path(subprocess.run(
        ["git", "rev-parse", "--show-toplevel"],
        capture_output=True, text=True, check=True,
    ).stdout.strip())

    failures = selftest()
    if failures:
        print("the detector itself is broken, so a clean scan would mean nothing:")
        for f in failures:
            print(f"  {f}")
        return 1
    print("detector self-test: ok")

    bad = scan(root)
    if bad:
        print("operator coordinates must be GitHub Actions variables, not repository "
              "content (#355, #356, #514):")
        for b in bad:
            print(f"  {b}")
        return 1
    print(f"ok — {len(tracked_docs(root))} documents carry no operator coordinates")
    return 0


if __name__ == "__main__":
    sys.exit(main())
