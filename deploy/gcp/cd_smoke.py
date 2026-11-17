#!/usr/bin/env python3
"""Verify the Console-issued CD credential through the staging HTTPS origin."""
import argparse
from pathlib import Path
import re
import sys
import time
import urllib.request

from native_edge import manifest
from native_smoke import NoRedirect, expect_response, json_object, require_auth_refusal, require_gateway_disabled, read_response


def probe(fetch, key, modelgateway=True, timeout=600, clock=time.monotonic, pause=time.sleep):
    public = '/v1/agents?limit=1'
    deadline = clock() + timeout
    def check(path, headers, expected):
        _, body = expect_response(lambda path, timeout: fetch(path, headers, timeout), path,
                                  expected, deadline=deadline, clock=clock, pause=pause)
        if expected == 401:
            require_auth_refusal(body)
        elif expected == 200:
            # internal/api/agents.go returns pageJSON (page.go), not a Files page.
            value = json_object(body)
            if (not isinstance(value.get('data'), list)
                    or 'next_page' in value and not isinstance(value['next_page'], str)
                    or any(not isinstance(item, dict) or item.get('type') != 'agent'
                           or not isinstance(item.get('id'), str) or not item['id'].startswith('agent_')
                           for item in value['data'])):
                raise RuntimeError('HTTPS public API did not return an agents page')
        else:
            require_gateway_disabled(body)
    # An accidentally open API is an immediate failure, before spending the key.
    check(public, {}, 401)
    for headers in ({'x-api-key': key}, {'Authorization': 'Bearer ' + key}):
        check(public, headers, 200)
        for path in ('/api/auth/session', '/api/console/organizations/default/service_accounts'):
            check(path, headers, 401)
        check('/admin/v1/profiles', headers, 401 if modelgateway else 503)


def read_key(path):
    # Source: internal/api/apikeys.go mints 32 random bytes with this public prefix.
    # Refuse whitespace or a shell/config escape without ever echoing the value.
    with Path(path).open('rb') as stream:
        value = stream.read(128)
    if not re.fullmatch(rb'sk-map-api01-[A-Za-z0-9_-]{43}', value):
        raise ValueError('CD key file must contain one Console-issued API key without whitespace')
    return value.decode('ascii')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--origin', required=True)
    parser.add_argument('--key-file', required=True)
    parser.add_argument('--modelgateway', choices=('true', 'false'), default='true')
    args = parser.parse_args()
    try:
        # Reuse the edge's canonical HTTPS DNS-origin rule before reading a key.
        manifest(args.origin, 'validation-only')
        key = read_key(args.key_file)
    except (ValueError, OSError):
        parser.error('a canonical HTTPS origin and a readable Console-issued CD key file are required')
    # Certificate verification stays on. Never forward a credential on a redirect.
    opener = urllib.request.build_opener(NoRedirect)
    def fetch(path, headers, timeout):
        request = urllib.request.Request(args.origin + path, headers=headers)
        return read_response(opener, request, timeout, limit=16 << 20)
    try:
        probe(fetch, key, modelgateway=args.modelgateway == 'true')
    except OSError:
        sys.exit('CD HTTPS check failed: transport error')
    except RuntimeError as error:
        sys.exit(f'CD HTTPS check failed: {error}')
    print('ok: issued CD key reaches public API via both headers; anonymous and private admission refused')


if __name__ == '__main__':
    main()
