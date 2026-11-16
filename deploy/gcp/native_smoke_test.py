#!/usr/bin/env python3
"""The public native check fails on stale IAP, unhealthy edges and wrong login."""
import unittest
from native_smoke import probe


class Smoke(unittest.TestCase):
    def test_disabled_gateway_requires_unavailable_admin_lane(self):
        responses = {
            "/api/auth/config": (200, b'{"mode":"oidc","native_login":true}'),
            "/v1/agents?limit=1": (401, b'{}'),
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

    def test_accepts_only_native_oidc_and_both_guarded_api_lanes(self):
        responses = {
            "/api/auth/config": (200, b'{"mode":"oidc","native_login":true}'),
            "/v1/agents?limit=1": (401, b'{}'),
            "/admin/v1/profiles": (401, b'{}'),
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
            ("/v1/agents?limit=1", (200, b'{}')),
            ("/v1/agents?limit=1", (500, b'{}')),
            ("/admin/v1/profiles", (200, b'{}')),
            ("/admin/v1/profiles", (404, b'{}')),
        ]:
            with self.subTest(path=path, wrong=wrong), self.assertRaises(RuntimeError):
                probe(lambda requested: wrong if requested == path else responses[requested])


if __name__ == "__main__":
    unittest.main(verbosity=2)
