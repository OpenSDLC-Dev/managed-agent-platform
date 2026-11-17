#!/usr/bin/env python3
"""The public native check fails on stale IAP, unhealthy edges and wrong login."""
import unittest
import urllib.error
import ssl
import signal
import time
import http.client
from email.message import Message
from unittest.mock import Mock
from native_smoke import probe, expect_response, read_response

AUTH = b'{"type":"error","error":{"type":"authentication_error","message":"required"}}'


class Smoke(unittest.TestCase):
    def test_request_timer_bounds_body_drips_and_restores_the_handler(self):
        opener = Mock()
        response = Mock()
        response.headers = Message()
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        response.read.side_effect = lambda _: time.sleep(3)
        opener.open.return_value = response
        previous = signal.getsignal(signal.SIGALRM)
        started = time.monotonic()
        with self.assertRaises(TimeoutError):
            read_response(opener, 'https://console.example/', 0.1)
        self.assertLess(time.monotonic() - started, 2)
        response.__exit__.assert_called_once()
        self.assertEqual(signal.getsignal(signal.SIGALRM), previous)
        self.assertEqual(signal.getitimer(signal.ITIMER_REAL), (0.0, 0.0))

    def test_retry_has_a_deadline_and_tls_failure_is_terminal(self):
        now = [0]
        waits = []
        def pause(seconds):
            waits.append(seconds)
            now[0] += seconds
        with self.assertRaisesRegex(RuntimeError, 'did not become ready'):
            expect_response(lambda *_: (503, b''), '/v1/agents?limit=1', 401,
                            timeout=7, clock=lambda: now[0], pause=pause)
        self.assertEqual(waits, [5, 2])
        def bad_tls(*_):
            raise urllib.error.URLError(ssl.SSLCertVerificationError('untrusted'))
        with self.assertRaisesRegex(RuntimeError, 'certificate verification failed'):
            expect_response(bad_tls, '/v1/agents?limit=1', 401,
                            pause=lambda _: self.fail('retried untrusted TLS'))

    def test_transient_retry_never_hides_an_authentication_failure(self):
        for transient in (502, 503, 504, urllib.error.URLError("unreachable")):
            now = [0]
            responses = [transient, 401]
            def fetch(path, timeout):
                value = responses.pop(0)
                if isinstance(value, Exception):
                    raise value
                return value, b'{}'
            def pause(seconds):
                now[0] += seconds
            expect_response(fetch, "/v1/agents?limit=1", 401,
                            clock=lambda: now[0], pause=pause)
            self.assertEqual(responses, [])
            self.assertEqual(now[0], 5)
        for status in (200, 301, 302, 400, 403, 404, 429, 500):
            calls = []
            with self.subTest(status=status), self.assertRaises(RuntimeError):
                expect_response(lambda path, timeout: (calls.append(path) or status, b'{}'),
                                "/v1/agents?limit=1", 401,
                                pause=lambda _: self.fail("retried unsafe status"))
            self.assertEqual(calls, ["/v1/agents?limit=1"])

    def test_disabled_gateway_requires_unavailable_admin_lane(self):
        responses = {
            "/api/auth/config": (200, b'{"mode":"oidc","native_login":true}'),
            "/v1/agents?limit=1": (401, AUTH),
            "/admin/v1/profiles": (503, b'gateway unavailable'),
        }
        visited = []

        def fetch(path):
            visited.append(path)
            return responses[path]

        probe(fetch, modelgateway=False)
        self.assertEqual(set(visited), set(responses))
        for status in (200, 302, 401, 404, 500, 502):
            with self.subTest(status=status), self.assertRaises(RuntimeError):
                probe(lambda path: (status, b'') if path == "/admin/v1/profiles"
                      else responses[path], modelgateway=False)
        with self.assertRaisesRegex(RuntimeError, 'disabled gateway route'):
            probe(lambda path: (503, b'<html>unhealthy backend</html>') if path == '/admin/v1/profiles'
                  else responses[path], modelgateway=False)

    def test_accepts_only_native_oidc_and_both_guarded_api_lanes(self):
        responses = {
            "/api/auth/config": (200, b'{"mode":"oidc","native_login":true}'),
            "/v1/agents?limit=1": (401, AUTH),
            "/admin/v1/profiles": (401, AUTH),
        }
        visited = []

        def fetch(path):
            visited.append(path)
            return responses[path]

        probe(fetch)
        self.assertEqual(set(visited), set(responses))
        for path, wrong in [
            ("/api/auth/config", (302, b'')),  # old IAP backend
            ("/api/auth/config", (502, b'bad gateway')),
            ("/api/auth/config", (200, b'<html>old Console</html>')),
            ("/api/auth/config", (200, b'{"mode":"disabled","native_login":false}')),
            ("/api/auth/config", (200, b'{"mode":"password","native_login":true}')),
            ("/api/auth/config", (200, b'{"mode":"oidc","native_login":1}')),
            ("/v1/agents?limit=1", (401, b'<html>old IAP</html>')),
            ("/admin/v1/profiles", (401, b'{}')),
            ("/v1/agents?limit=1", (200, b'{}')),
            ("/v1/agents?limit=1", (500, b'{}')),
            ("/admin/v1/profiles", (200, b'{}')),
            ("/admin/v1/profiles", (404, b'{}')),
        ]:
            with self.subTest(path=path, wrong=wrong), self.assertRaises(RuntimeError):
                probe(lambda requested: wrong if requested == path else responses[requested])

    def test_deadline_prevents_a_new_request_and_rejects_a_late_success(self):
        for initial, increment in ((7, 0), (0, 7)):
            now = [initial]
            calls = []
            def fetch(path, timeout):
                calls.append(timeout)
                now[0] += increment
                return 401, AUTH
            with self.assertRaisesRegex(RuntimeError, 'did not become ready'):
                expect_response(fetch, '/v1/agents?limit=1', 401, deadline=7,
                                clock=lambda: now[0])
            self.assertEqual(calls, [] if initial == 7 else [7])

    def test_transport_rejects_iap_and_redacts_malformed_framing(self):
        opener = Mock()
        opener.open.side_effect = http.client.BadStatusLine('reflected-sensitive-value')
        with self.assertRaises(RuntimeError) as caught:
            read_response(opener, 'https://console.example/', 1)
        self.assertNotIn('reflected-sensitive-value', str(caught.exception))
        self.assertTrue(caught.exception.__suppress_context__)
        opener.open.side_effect = None
        response = Mock()
        response.headers = Message()
        response.headers['X-Goog-IAP-Generated-Response'] = ''
        response.__enter__ = Mock(return_value=response)
        response.__exit__ = Mock(return_value=False)
        opener.open.return_value = response
        with self.assertRaisesRegex(RuntimeError, 'retired IAP'):
            read_response(opener, 'https://console.example/', 1)
        response.read.assert_not_called()


if __name__ == "__main__":
    unittest.main(verbosity=2)
