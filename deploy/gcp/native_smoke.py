#!/usr/bin/env python3
"""Check the reconciled HTTPS native edge, without a browser credential."""
import argparse
import json
import sys
import time
import urllib.error
import urllib.request


def probe(fetch, modelgateway=True):
    status, body = fetch("/api/auth/config")
    if status != 200:
        raise RuntimeError(f"native auth config returned {status}, expected 200")
    try:
        config = json.loads(body)
    except ValueError:
        raise RuntimeError("native auth config was not JSON") from None
    if config != {"mode": "oidc", "native_login": True}:
        raise RuntimeError("HTTPS origin does not serve native OIDC")
    checks = (("/v1/agents?limit=1", 401),
              ("/admin/v1/profiles", 401 if modelgateway else 503))
    for path, expected in checks:
        status, _ = fetch(path)
        if status != expected:
            raise RuntimeError(f"anonymous {path} returned {status}, expected {expected}")


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        return None


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True)
    parser.add_argument("--modelgateway", choices=("true", "false"), default="true")
    args = parser.parse_args()
    if not args.origin.startswith("https://"):
        parser.error("origin must use HTTPS")
    # urllib verifies certificates by default. An IAP redirect must fail here,
    # not be followed to a Google page whose 200 could hide the stale backend.
    opener = urllib.request.build_opener(NoRedirect)

    def fetch(path):
        try:
            response = opener.open(args.origin + path, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            return response.status, response.read(16384)

    deadline = time.monotonic() + 600
    while True:
        try:
            probe(fetch, modelgateway=args.modelgateway == "true")
            break
        except (OSError, RuntimeError) as error:
            if time.monotonic() >= deadline:
                sys.exit(f"native HTTPS edge did not become ready: {error}")
            time.sleep(5)
    print("ok: verified HTTPS, native OIDC config, anonymous public/admin API refusals")
