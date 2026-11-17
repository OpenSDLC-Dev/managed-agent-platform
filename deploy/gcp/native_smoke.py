#!/usr/bin/env python3
"""Check the reconciled HTTPS native edge, without a browser credential."""
import argparse
from contextlib import contextmanager
import http.client
import json
import signal
import ssl
import sys
import time
import urllib.error
import urllib.request


def expect_response(fetch, path, expected, timeout=600, clock=time.monotonic, pause=time.sleep, deadline=None):
    """Retry edge convergence only; an unexpected auth answer fails immediately."""
    if deadline is None:
        deadline = clock() + timeout
    while True:
        remaining = deadline - clock()
        if remaining <= 0:
            raise RuntimeError(f"{path} did not become ready; expected {expected}")
        status = None
        try:
            status, body = fetch(path, min(10, remaining))
        except urllib.error.URLError as error:
            if isinstance(error.reason, ssl.SSLError):
                raise RuntimeError("HTTPS certificate verification failed") from None
        except (TimeoutError, ConnectionError):
            pass
        if clock() >= deadline:
            raise RuntimeError(f"{path} did not become ready; expected {expected}")
        if status == expected:
            return status, body
        if status is not None and status not in (502, 503, 504):
            raise RuntimeError(f"{path} returned {status}, expected {expected}")
        remaining = deadline - clock()
        if remaining <= 0:
            raise RuntimeError(f"{path} did not become ready; expected {expected}")
        pause(min(5, remaining))


def json_object(body):
    try:
        value = json.loads(body)
    except ValueError:
        raise RuntimeError("HTTPS response was not a JSON object") from None
    if not isinstance(value, dict):
        raise RuntimeError("HTTPS response was not a JSON object")
    return value


def require_auth_refusal(body):
    # CP errors.go and gateway admin/admin.go share this envelope; request_id differs.
    value = json_object(body)
    error = value.get('error')
    if (value.get('type') != 'error' or not isinstance(error, dict)
            or error.get('type') != 'authentication_error' or not isinstance(error.get('message'), str)):
        raise RuntimeError("HTTPS response was not a native authentication refusal")


def require_gateway_disabled(body):
    # The chart and Compose share files/app-edge/admin-disabled.caddy verbatim.
    if body != b'gateway unavailable':
        raise RuntimeError("HTTPS response was not the disabled gateway route")


@contextmanager
def request_deadline(seconds):
    # These CD scripts run on the Linux runner (locally, Bash/WSL). A socket
    # inactivity timeout cannot bound a peer that keeps dripping header/body bytes.
    if not hasattr(signal, 'setitimer'):
        raise RuntimeError('HTTPS smoke checks require a POSIX runtime, such as WSL')
    if signal.getitimer(signal.ITIMER_REAL) != (0.0, 0.0):
        raise RuntimeError('HTTPS smoke checks require an unused real-time timer')
    def expired(*_):
        raise TimeoutError('HTTPS request deadline exceeded')
    previous = signal.signal(signal.SIGALRM, expired)
    try:
        signal.setitimer(signal.ITIMER_REAL, seconds)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous)


def read_response(opener, request, timeout, limit=16384):
    """Do not expose parser text or accept the retired IAP backend's response."""
    try:
        with request_deadline(timeout):
            try:
                response = opener.open(request, timeout=timeout)
            except urllib.error.HTTPError as error:
                response = error
            with response:
                if 'x-goog-iap-generated-response' in response.headers:
                    raise RuntimeError("HTTPS response came from the retired IAP backend")
                body = response.read(limit + 1)
                if len(body) > limit:
                    raise RuntimeError("HTTPS response exceeded the smoke-check size limit")
                return response.status, body
    except http.client.HTTPException:
        # BadStatusLine embeds origin-supplied bytes, which could echo our key.
        raise RuntimeError("HTTPS response had invalid HTTP framing") from None


def probe(fetch, modelgateway=True):
    status, body = fetch("/api/auth/config")
    if status != 200:
        raise RuntimeError(f"native auth config returned {status}, expected 200")
    config = json_object(body)
    if config != {"mode": "oidc", "native_login": True} or config.get('native_login') is not True:
        raise RuntimeError("HTTPS origin does not serve native OIDC")
    checks = (("/v1/agents?limit=1", 401),
              ("/admin/v1/profiles", 401 if modelgateway else 503))
    for path, expected in checks:
        status, body = fetch(path)
        if status != expected:
            raise RuntimeError(f"anonymous {path} returned {status}, expected {expected}")
        if expected == 401:
            require_auth_refusal(body)
        else:
            require_gateway_disabled(body)


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

    def fetch(path, timeout):
        return read_response(opener, args.origin + path, timeout)

    deadline = time.monotonic() + 600
    def ready(path):
        expected = (200 if path == "/api/auth/config" else
                    503 if path == "/admin/v1/profiles" and args.modelgateway == "false" else 401)
        return expect_response(fetch, path, expected, deadline=deadline)

    try:
        probe(ready, modelgateway=args.modelgateway == "true")
    except OSError:
        sys.exit("native HTTPS edge check failed: transport error")
    except RuntimeError as error:
        sys.exit(f"native HTTPS edge check failed: {error}")
    print("ok: verified HTTPS, native OIDC config, anonymous public/admin API refusals")
