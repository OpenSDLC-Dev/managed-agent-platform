#!/usr/bin/env python3
"""The native staging edge reuses TLS and leaves admission to the platform."""
import unittest

from native_edge import manifest


class NativeEdge(unittest.TestCase):
    def test_keeps_tls_and_routes_all_paths_to_the_native_edge(self):
        items = {item["kind"]: item for item in manifest("https://console.example.com", "map-managed-agent-platform-app-edge")["items"]}
        ingress = items["Ingress"]
        self.assertEqual(ingress["metadata"]["name"], "console")
        self.assertEqual(ingress["metadata"]["annotations"]["kubernetes.io/ingress.global-static-ip-name"], "console-ip")
        self.assertEqual(ingress["metadata"]["annotations"]["networking.gke.io/managed-certificates"], "console")
        rule, = ingress["spec"]["rules"]
        self.assertEqual(rule["host"], "console.example.com")
        path, = rule["http"]["paths"]
        self.assertEqual(path["path"], "/*")
        self.assertEqual(path["backend"]["service"], {"name": "map-managed-agent-platform-app-edge", "port": {"number": 8080}})
        self.assertEqual(items["ManagedCertificate"]["spec"]["domains"], ["console.example.com"])
        self.assertEqual(items["BackendConfig"]["spec"], {
            "iap": {"enabled": False}, "timeoutSec": 3600,
            "healthCheck": {"type": "HTTP", "requestPath": "/api/health", "port": 8080},
        })
        self.assertTrue(items["FrontendConfig"]["spec"]["redirectToHttps"]["enabled"])

    def test_refuses_an_origin_google_cannot_redirect_to_or_a_bad_service(self):
        for origin in ["", "http://console.example.com", "https://127.0.0.1", "https://[::1]", "https://console.example.com/", "https://console.example.com:443", "https://console.example.com:8443", "https://CONSOLE.example.com", "https://a@console.example.com", "https://console.example.com?x=1", "https://console.example.com#fragment"]:
            with self.subTest(origin=origin), self.assertRaises(ValueError):
                manifest(origin, "map-app-edge")
        for service in ["", "a/b", "a\nkind: Secret", "-bad", "a" * 64]:
            with self.subTest(service=service), self.assertRaises(ValueError):
                manifest("https://console.example.com", service)


if __name__ == "__main__":
    unittest.main(verbosity=2)
