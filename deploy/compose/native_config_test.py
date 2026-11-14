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
    def test_native_and_legacy_isolate_credentials_and_ports(self):
        for mode in ["", "platform"]:
            for project in ["mapnativeone", "mapnativetwo"]:
                env = {key: os.environ[key] for key in ["PATH", "HOME"] if key in os.environ}
                env.update(CONTROLPLANE_API_KEY="render-only-key", BRAIN_API_KEY="render-only-brain", CONSOLE_AUTH_MODE=mode)
                if mode:
                    env.update(IDENTITY_MODE="password", DEVELOPMENT_MODE="true", CONSOLE_PUBLIC_ORIGIN="http://localhost:3000")
                result = subprocess.run(["docker", "compose", "--env-file", "/dev/null", "-f", str(ROOT / "docker-compose.yml"), "-p", project, "--profile", "console", "--profile", "iam", "config", "--format", "json"], env=env, capture_output=True, text=True, check=True)
                services = json.loads(result.stdout)["services"]
                cp, gw, console, edge = [services[key] for key in ["controlplane", "modelgateway", "console", "app-edge"]]
                self.assertEqual(console["image"], "ghcr.io/opensdlc-dev/managed-agent-console@sha256:a6a7285c3aef10bd12f62d0c1fb51a1b878e654b4c09416af0891833941b4987")
                self.assertIn("@sha256:", edge["image"])
                for service in [console, edge]:
                    self.assertNotIn("build", service)
                    for key in ["DATABASE_URL", "CONTROLPLANE_API_KEY", "BRAIN_API_KEY", "IDENTITY_OIDC_CLIENT_SECRET"]:
                        self.assertNotIn(key, service["environment"])
                self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", gw["environment"])
                self.assertIn("IDENTITY_OIDC_CLIENT_SECRET", cp["environment"])
                self.assertEqual(console["environment"]["PLATFORM_API_KEY"], "")
                self.assertEqual(edge["ports"][0]["host_ip"], "127.0.0.1")
                self.assertEqual(str(edge["ports"][0]["published"]), "3000")
                self.assertNotIn("ports", console)
                self.assertEqual(cp["ports"][0]["target"], 8080)
                self.assertEqual(gw["ports"][0]["target"], 8090)
                self.assertEqual(console["environment"]["CONSOLE_AUTH_MODE"], mode)
                self.assertEqual(edge["environment"]["CONSOLE_AUTH_MODE"], mode)
                self.assertEqual(cp["environment"]["CONSOLE_PUBLIC_ORIGIN"], gw["environment"]["CONSOLE_PUBLIC_ORIGIN"])
                self.assertIn("idp.localhost", services["idp"]["networks"]["default"]["aliases"])

    def test_startup_refuses_crossed_modes_before_starting_a_server(self):
        cases = [
            ("app-edge/start.sh", {"CONSOLE_AUTH_MODE": "wrong"}, "must be empty or platform"),
            ("app-edge/start.sh", {"CONSOLE_PUBLIC_ORIGIN": "https://console.example"}, "legacy routing cannot"),
            ("app-edge/start.sh", {"CONSOLE_AUTH_MODE": "platform"}, "requires CONSOLE_PUBLIC_ORIGIN"),
            ("app-edge/start.sh", {"CONSOLE_AUTH_MODE": "platform", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_MODE": "disabled"}, "requires an enabled native identity"),
            ("console-start.sh", {"CONSOLE_AUTH_MODE": "platform", "PLATFORM_API_KEY": "render-only-key"}, "must not receive legacy credentials"),
            ("console-start.sh", {"CONSOLE_AUTH_MODE": "wrong"}, "must be empty or platform"),
        ]
        with tempfile.TemporaryDirectory() as directory:
            # Unit-only executables make a missing guard return success, not an
            # unrelated command-not-found failure. Real Caddy has its own suite.
            for name in ["caddy", "node"]:
                executable = Path(directory) / name
                executable.write_text('#!/bin/sh\nprintf "%s|%s\\n" "${APP_EDGE_MODE:-}" "${APP_EDGE_ADMIN:-}"\n')
                executable.chmod(0o700)
            for script, env, message in cases:
                with self.subTest(script=script, env=env):
                    result = subprocess.run(["/bin/sh", str(ROOT / script)], env={**env, "PATH": directory}, capture_output=True, text=True)
                    self.assertEqual(result.returncode, 1)
                    self.assertIn(message, result.stderr)
                    self.assertNotIn("render-only-key", result.stderr)
            for env, expected in [({}, "legacy|disabled"), ({"CONSOLE_AUTH_MODE": "platform", "CONSOLE_PUBLIC_ORIGIN": "https://console.example", "IDENTITY_MODE": "oidc", "MODELGATEWAY_UPSTREAM": "gateway:8090"}, "native|enabled")]:
                result = subprocess.run(["/bin/sh", str(ROOT / "app-edge/start.sh")], env={**env, "PATH": directory}, capture_output=True, text=True)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stdout.strip(), expected)

    def test_chart_caddy_files_are_byte_identical(self):
        chart = ROOT.parent / "helm/managed-agent-platform/files/app-edge"
        names = {p.name for p in (ROOT / "app-edge").iterdir() if p.is_file()}
        self.assertEqual(names, {p.name for p in chart.iterdir() if p.is_file()})
        for name in names:
            self.assertEqual((ROOT / "app-edge" / name).read_bytes(), (chart / name).read_bytes(), name)


if __name__ == "__main__":
    unittest.main(verbosity=2)
