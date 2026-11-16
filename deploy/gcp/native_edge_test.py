#!/usr/bin/env python3
"""The native staging edge reuses TLS and leaves admission to the platform."""
import os
import copy
from pathlib import Path
import subprocess
import tempfile
import textwrap
import unittest

import native_edge

ROOT = Path(__file__).resolve().parents[2]


def workflow_step(name):
    lines = (ROOT / ".github/workflows/deploy.yml").read_text().splitlines()
    start = lines.index("      - name: " + name)
    end = next((i for i in range(start + 1, len(lines))
                if lines[i].startswith("      - ")), len(lines))
    block = lines[start:end]
    run = block.index("        run: |")
    body = []
    for line in block[run + 1:]:
        if line.strip() and not line.startswith("          "):
            break
        body.append(line)
    return start, block, textwrap.dedent("\n".join(body))


class NativeDeployPreflight(unittest.TestCase):
    def test_parked_deploy_does_not_require_unconfigured_native_login(self):
        env = {"PATH": os.environ["PATH"]}
        for name in ("GCP_PROJECT_ID", "GCP_ZONE", "GKE_CLUSTER", "WIF_PROVIDER",
                     "DEPLOY_SERVICE_ACCOUNT", "BLOB_BUCKET", "KMS_KEY_NAME",
                     "CONTROLPLANE_SERVICE_ACCOUNT", "BRAIN_SERVICE_ACCOUNT",
                     "EXECUTOR_SERVICE_ACCOUNT"):
            env[name] = "render-only"
        env["ARTIFACT_REGISTRY"] = "registry.invalid/project/repository"

        def run(script, extra):
            return subprocess.run(["bash", "-e", "-c", script], env={**env, **extra},
                                  cwd=ROOT, capture_output=True, text=True)

        _, _, coordinates = workflow_step("Require the deployment variables")
        result = run(coordinates, {})
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        parked, _, _ = workflow_step("A parked environment is skipped rather than failed")
        native, block, preflight = workflow_step("Require native login settings for a live deployment")
        self.assertGreater(native, parked)
        self.assertIn("        if: env.POWERED_OFF != 'true'", block)
        self.assertNotEqual(run(preflight, {}).returncode, 0)
        valid = {"STAGING_PUBLIC_ORIGIN": "https://console.example.com",
                 "OIDC_CLIENT_ID": "test.apps.googleusercontent.com",
                 "OIDC_CLIENT_SECRET_NAME": "test-oidc-secret"}
        result = run(preflight, valid)
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        for name in valid:
            for value in ("", " ", "bad,value"):
                self.assertNotEqual(run(preflight, {**valid, name: value}).returncode, 0)
        self.assertNotEqual(run(preflight, {**valid, "STAGING_PUBLIC_ORIGIN":
                                           "http://console.example.com"}).returncode, 0)

    def test_public_smoke_sends_its_credential_only_over_https(self):
        _, _, script = workflow_step("Smoke — the public API answers")
        with tempfile.TemporaryDirectory() as directory:
            scratch = Path(directory)
            (scratch / "controlplane-api-key").write_text("fixture-only-key")
            for name, body in {
                "kubectl": "printf '192.0.2.1\\n'",
                "sleep": ":",
                "curl": '''printf '%s\\n' "$*" >> "$SMOKE_CALLS"
case "$*" in
  *https://console.example.com/v1/agents?limit=1*) ;;
  *) exit 1 ;;
esac
case " $* " in *' -K '*) printf 200 ;; *) printf 401 ;; esac''',
            }.items():
                executable = scratch / name
                executable.write_text("#!/bin/sh\n" + body + "\n")
                executable.chmod(0o700)
            env = {"PATH": directory + os.pathsep + os.environ["PATH"],
                   "SECRET_DIR": directory, "SMOKE_CALLS": str(scratch / "calls"),
                   "STAGING_PUBLIC_ORIGIN": "https://console.example.com",
                   "K8S_NAMESPACE": "fixture", "HELM_RELEASE": "fixture", "GITHUB_SHA": "fixture"}
            result = subprocess.run(["bash", "-e", "-c", script], env=env,
                                    cwd=ROOT, capture_output=True, text=True)
            self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
            calls = (scratch / "calls").read_text()
            self.assertEqual(len(calls.splitlines()), 2)
            self.assertNotIn("fixture-only-key", calls + result.stdout + result.stderr)


class NativeEdge(unittest.TestCase):
    def test_cutover_refuses_changes_to_the_existing_origin_or_tls_assets(self):
        existing = {"items": [
            {"kind": "Ingress", "metadata": {"name": "console", "annotations": {
                "kubernetes.io/ingress.global-static-ip-name": "console-ip",
                "networking.gke.io/managed-certificates": "console",
                "networking.gke.io/v1beta1.FrontendConfig": "console",
            }}, "spec": {"rules": [{"host": "console.example.com"}]}},
            {"kind": "ManagedCertificate", "metadata": {"name": "console"},
             "spec": {"domains": ["console.example.com"]}},
        ]}
        native_edge.validate_existing("https://console.example.com", existing)
        with self.assertRaises(ValueError):
            native_edge.validate_existing("https://typo.example.com", existing)
        for item in existing["items"]:
            with self.assertRaises(ValueError):
                native_edge.validate_existing("https://console.example.com", {"items": [item]})
        for annotation in existing["items"][0]["metadata"]["annotations"]:
            wrong = copy.deepcopy(existing)
            wrong["items"][0]["metadata"]["annotations"][annotation] = "another-resource"
            with self.subTest(annotation=annotation), self.assertRaises(ValueError):
                native_edge.validate_existing("https://console.example.com", wrong)
        for domains in ([], ["wrong.example.com"], ["console.example.com", "extra.example.com"]):
            wrong = copy.deepcopy(existing)
            wrong["items"][1]["spec"]["domains"] = domains
            with self.subTest(domains=domains), self.assertRaises(ValueError):
                native_edge.validate_existing("https://console.example.com", wrong)

    def test_keeps_tls_and_routes_all_paths_to_the_native_edge(self):
        items = {item["kind"]: item for item in native_edge.manifest("https://console.example.com", "map-managed-agent-platform-app-edge")["items"]}
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
                native_edge.manifest(origin, "map-app-edge")
        for service in ["", "a/b", "a\nkind: Secret", "-bad", "a" * 64]:
            with self.subTest(service=service), self.assertRaises(ValueError):
                native_edge.manifest("https://console.example.com", service)


if __name__ == "__main__":
    unittest.main(verbosity=2)
