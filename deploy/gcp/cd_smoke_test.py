#!/usr/bin/env python3
"""CD uses an issued key on HTTPS public APIs, never browser administration."""
import unittest
from pathlib import Path
import tempfile
from unittest.mock import patch

from cd_smoke import probe, read_key, main

AUTH = b'{"type":"error","error":{"type":"authentication_error","message":"required"}}'
PAGE = b'{"data":[]}'


class CDSmoke(unittest.TestCase):
    def test_disabled_gateway_requires_its_explicit_edge_response(self):
        body = [b'gateway unavailable']
        def fetch(path, headers, timeout):
            if path == '/admin/v1/profiles':
                return 503, body[0]
            return (200, PAGE) if path.startswith('/v1/') and headers else (401, AUTH)
        probe(fetch, 'synthetic-key', modelgateway=False)
        body[0] = b'<html>unhealthy backend</html>'
        with self.assertRaisesRegex(RuntimeError, 'disabled gateway route'):
            probe(fetch, 'synthetic-key', modelgateway=False)

    def test_bad_auth_answer_never_retries_or_leaks_the_key(self):
        key = 'sk-map-api01-' + 'k' * 43
        for broken_path, broken_headers, status in (
            ('/v1/agents?limit=1', {}, 200),
            ('/v1/agents?limit=1', {'x-api-key': key}, 401),
            ('/api/auth/session', {'x-api-key': key}, 200),
            ('/admin/v1/profiles', {'Authorization': 'Bearer ' + key}, 200),
            ('/v1/agents?limit=1', {'x-api-key': key}, 302),
        ):
            visits = []
            def fetch(path, headers, timeout):
                visits.append((path, headers))
                if (path, headers) == (broken_path, broken_headers):
                    return status, b''
                return (200, PAGE) if path.startswith('/v1/') and headers else (401, AUTH)
            with self.subTest(path=broken_path, status=status):
                with self.assertRaises(RuntimeError) as caught:
                    probe(fetch, key)
                self.assertNotIn(key, str(caught.exception))
                self.assertEqual(visits[-1], (broken_path, broken_headers))
                self.assertEqual(visits.count((broken_path, broken_headers)), 1)

    def test_key_file_is_exact_and_errors_do_not_reveal_its_contents(self):
        key = b'sk-map-api01-' + b'k' * 43
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'key'
            path.write_bytes(key)
            self.assertEqual(read_key(path), key.decode())
            for invalid in (b'', key + b'\n', b' ' + key, key + b'"', b'opaque-value', b'x' * 1024):
                path.write_bytes(invalid)
                with self.assertRaises(ValueError) as caught:
                    read_key(path)
                self.assertNotIn(key.decode(), str(caught.exception))

    def test_cli_rejects_noncanonical_origins_before_reading_the_key(self):
        for origin in ('http://console.example', 'https://user@console.example',
                       'https://console.example/path', 'https://console.example?x=1'):
            with self.subTest(origin=origin), patch('sys.argv', ['cd_smoke.py', '--origin', origin, '--key-file', 'unused']), \
                    patch('cd_smoke.read_key') as reader, patch('sys.stderr'):
                with self.assertRaises(SystemExit) as caught:
                    main()
                self.assertEqual(caught.exception.code, 2)
                reader.assert_not_called()

    def test_owned_key_reaches_public_api_with_both_headers_only(self):
        key = 'sk-map-api01-' + 'k' * 43
        visits = []
        def fetch(path, headers, timeout):
            visits.append((path, headers))
            return (200, PAGE) if path.startswith('/v1/') and headers else (401, AUTH)
        probe(fetch, key)
        self.assertIn(('/v1/agents?limit=1', {'x-api-key': key}), visits)
        self.assertIn(('/v1/agents?limit=1', {'Authorization': 'Bearer ' + key}), visits)
        self.assertIn(('/v1/agents?limit=1', {}), visits)
        for path in ('/api/auth/session', '/api/console/organizations/default/service_accounts', '/admin/v1/profiles'):
            for headers in ({'x-api-key': key}, {'Authorization': 'Bearer ' + key}):
                self.assertIn((path, headers), visits)
        self.assertEqual(len(visits), 9)

    def test_status_alone_never_proves_platform_admission(self):
        for body in (b'<html>old edge</html>', b'{}', b'[]', b'{"data":null}',
                     b'{"data":[],"next_page":42}', b'{"data":[{"type":"file","id":"file_1"}]}'):
            def fetch(path, headers, timeout):
                return (200, body) if path.startswith('/v1/') and headers else (401, AUTH)
            with self.subTest(body=body), self.assertRaises(RuntimeError):
                probe(fetch, 'synthetic-key')
        with self.assertRaisesRegex(RuntimeError, 'not a native authentication refusal'):
            probe(lambda *_: (401, b'{}'), 'synthetic-key')

    def test_all_routes_share_one_deadline(self):
        now = [0]
        calls = []
        def fetch(path, headers, timeout):
            calls.append(timeout)
            now[0] += 4
            return (200, PAGE) if headers else (401, AUTH)
        with self.assertRaisesRegex(RuntimeError, 'did not become ready'):
            probe(fetch, 'synthetic-key', timeout=7, clock=lambda: now[0])
        self.assertEqual(calls, [7, 3])


if __name__ == '__main__':
    unittest.main(verbosity=2)
