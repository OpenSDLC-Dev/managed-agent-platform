#!/usr/bin/env python3
"""Validate the real Compose model without reading .env or starting services."""
import json
import os
import subprocess
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent


class ComposeConfig(unittest.TestCase):
    def render(self, *args):
        env = {key: os.environ[key] for key in ["PATH", "HOME"] if key in os.environ}
        result = subprocess.run(["docker", "compose", "--env-file", "/dev/null", "-f", str(ROOT / "docker-compose.yml"), *args, "config", "--format", "json"], env=env, capture_output=True, text=True, check=True)
        return json.loads(result.stdout)["services"]

    def test_default_install_uses_password_login_and_claim_tokens(self):
        services = self.render("--profile", "console")
        for name in ["controlplane", "modelgateway", "app-edge"]:
            self.assertEqual(services[name]["environment"]["IDENTITY_MODE"], "password")
            self.assertEqual(services[name]["environment"]["CONSOLE_PUBLIC_ORIGIN"], "http://localhost:3000")
        for name in ["controlplane", "modelgateway"]:
            self.assertEqual(services[name]["environment"]["DEVELOPMENT_MODE"], "true")
        self.assertFalse(any(key.endswith("API_KEY") for name in ["controlplane", "modelgateway", "brain"] for key in services[name]["environment"]))
        self.assertEqual(json.loads((ROOT / "model-providers.gateway.json").read_text()), [{
            "model": "*", "protocol": "anthropic", "base_url": "http://modelgateway:8090", "delegated_inference": True,
        }])

    def test_native_install_isolates_credentials_and_ports(self):
        for project in ["mapnativeone", "mapnativetwo"]:
            services = self.render("-p", project, "--profile", "console", "--profile", "iam")
            cp, gw, console, edge = [services[key] for key in ["controlplane", "modelgateway", "console", "app-edge"]]
            self.assertEqual(console["image"], "ghcr.io/opensdlc-dev/managed-agent-console@sha256:52d8f869977e1724204297f8f62252b3ec5f48a6ac2953d969a6897e5050ba65")
            self.assertIn("@sha256:", edge["image"])
            for service in [console, edge]:
                self.assertNotIn("build", service)
            self.assertFalse(console.get("environment"))
            self.assertEqual(set(edge["environment"]), {"CONSOLE_PUBLIC_ORIGIN", "IDENTITY_MODE", "MODELGATEWAY_UPSTREAM", "APP_EDGE_CLIENT_IP_TOKEN"})
            self.assertTrue(edge["environment"]["APP_EDGE_CLIENT_IP_TOKEN"])
            self.assertEqual(edge["environment"]["APP_EDGE_CLIENT_IP_TOKEN"], cp["environment"]["APP_EDGE_CLIENT_IP_TOKEN"])
            self.assertNotIn("APP_EDGE_CLIENT_IP_TOKEN", gw["environment"])
            self.assertFalse(any(key.startswith("IDENTITY_") and key != "IDENTITY_MODE" for key in gw["environment"]))
            self.assertNotIn("SSL_CERT_FILE", gw["environment"])
            self.assertNotIn("volumes", gw)
            self.assertIn("IDENTITY_OIDC_CLIENT_SECRET", cp["environment"])
            self.assertEqual(edge["ports"][0]["host_ip"], "127.0.0.1")
            self.assertEqual(str(edge["ports"][0]["published"]), "3000")
            self.assertNotIn("ports", console)
            self.assertEqual(cp["ports"][0]["target"], 8080)
            self.assertEqual(gw["ports"][0]["target"], 8090)
            self.assertEqual(cp["environment"]["CONSOLE_PUBLIC_ORIGIN"], gw["environment"]["CONSOLE_PUBLIC_ORIGIN"])
            self.assertIn("idp.localhost", services["idp"]["networks"]["default"]["aliases"])

    def test_startup_requires_login_before_starting_a_server(self):
        cases = [
            ({}, "requires CONSOLE_PUBLIC_ORIGIN"),
            ({"CONSOLE_PUBLIC_ORIGIN": "https://console.example"}, "requires an enabled native identity"),
            ({"CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_MODE": "disabled"}, "requires an enabled native identity"),
        ]
        with tempfile.TemporaryDirectory() as directory:
            executable = Path(directory) / "caddy"
            executable.write_text('#!/bin/sh\nprintf "%s\\n" "${APP_EDGE_ADMIN:-}"\n')
            executable.chmod(0o700)
            for env, message in cases:
                with self.subTest(env=env):
                    result = subprocess.run(["/bin/sh", str(ROOT / "app-edge/start.sh")], env={**env, "PATH": directory}, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 1)
                    self.assertIn(message, result.stderr)
            for mode in ["password", "oidc", "trusted_proxy"]:
                for gateway in ["", "gateway:8090"]:
                    env = {"CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_MODE": mode, "MODELGATEWAY_UPSTREAM": gateway, "PATH": directory}
                    result = subprocess.run(["/bin/sh", str(ROOT / "app-edge/start.sh")], env=env, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 0, result.stderr)
                    self.assertEqual(result.stdout.strip(), "enabled" if gateway else "disabled")

    def test_chart_caddy_files_are_byte_identical(self):
        chart = ROOT.parent / "helm/managed-agent-platform/files/app-edge"
        names = {p.name for p in (ROOT / "app-edge").iterdir() if p.is_file()}
        self.assertEqual(names, {p.name for p in chart.iterdir() if p.is_file()})
        for name in names:
            self.assertEqual((ROOT / "app-edge" / name).read_bytes(), (chart / name).read_bytes(), name)


if __name__ == "__main__":
    unittest.main(verbosity=2)
