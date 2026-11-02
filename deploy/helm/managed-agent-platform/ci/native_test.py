#!/usr/bin/env python3
"""Credential-free one-origin render contracts. Helm remains the YAML validator."""
import base64
import json
import re
import subprocess
import tempfile
import textwrap
import unittest
from pathlib import Path

CHART = Path(__file__).resolve().parents[1]
ROOT = CHART.parents[2]
CONSOLE = "ghcr.io/opensdlc-dev/managed-agent-console@sha256:9b8d0408a4fbe4e1d1eac760473aa2ee0053c1c0ce56724923dde2709c43c3a9"


def render(values=None, release="native-test", success=True):
    with tempfile.TemporaryDirectory() as directory:
        path = Path(directory) / "values.json"
        path.write_text(json.dumps(values or {}))
        arguments = [str(CHART), "-f", str(CHART / "ci/example-values.yaml"), "-f", str(path)]
        if success:
            lint = subprocess.run(["helm", "lint", *arguments], capture_output=True, text=True)
            if lint.returncode:
                raise AssertionError(lint.stdout + lint.stderr)
        result = subprocess.run(["helm", "template", release, *arguments], capture_output=True, text=True)
    if success and result.returncode:
        raise AssertionError(result.stderr)
    if not success:
        if result.returncode == 0:
            raise AssertionError("invalid configuration rendered successfully")
        if "execution error" not in result.stderr:
            raise AssertionError("not a semantic validation refusal: " + result.stderr)
    return result.stdout if success else result.stderr


def section(text, name):
    matches = re.findall(r"# Source: [^\n]+/" + re.escape(name) + r"\n(.*?)(?=\n---|\Z)", text, re.S)
    return "\n".join(matches)


def native(mode="password"):
    return {"console": {"enabled": True, "authMode": "platform", "publicOrigin": "http://localhost:3000"},
            "identity": {"mode": mode}, "developmentMode": True}


class NativeRender(unittest.TestCase):
    def test_default_and_legacy(self):
        default = render()
        self.assertNotIn("CONSOLE_AUTH_MODE", default)
        legacy = render({"console": {"enabled": True}})
        self.assertIn(CONSOLE, section(legacy, "console.yaml"))
        self.assertIn("legacy", section(legacy, "app-edge.yaml"))
        self.assertIn("controlplane-api-key", section(legacy, "console.yaml"))
        self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", legacy)

    def test_password_and_env_isolation(self):
        text = render(native())
        for name in ["controlplane-deployment.yaml", "modelgateway-deployment.yaml"]:
            body = section(text, name)
            self.assertIn("CONSOLE_PUBLIC_ORIGIN", body)
            self.assertIn('"http://localhost:3000"', body)
            self.assertIn("DEVELOPMENT_MODE", body)
        for name in ["console.yaml", "app-edge.yaml"]:
            body = section(text, name)
            self.assertTrue(body, name)
            self.assertNotIn("DATABASE_URL", body)
            self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", body)
            self.assertNotIn("controlplane-api-key", body)
        self.assertIn("/api/health", section(text, "console.yaml"))
        self.assertNotIn("deep=", section(text, "console.yaml"))

    def test_oidc_public_confidential_and_existing_secret(self):
        for existing in [False, True]:
            for secret in ["", "render-only-confidential"]:
                values = native("oidc")
                values["console"]["publicOrigin"] = "https://console.example"
                values["identity"]["oidc"] = {"issuer": "https://idp.example", "audience": "client", "clientId": "client", "clientSecret": secret}
                if existing:
                    values.update(existingSecret="operator-secret", postgresql={"enabled": False}, minio={"enabled": False}, openbao={"enabled": False})
                    values["identity"]["oidc"]["clientSecret"] = ""
                    if secret:
                        values["identity"]["oidc"]["clientSecretKey"] = "confidential-rp"
                text = render(values)
                cp = section(text, "controlplane-deployment.yaml")
                self.assertIn("IDENTITY_OIDC_CLIENT_ID", cp)
                if secret:
                    self.assertIn("IDENTITY_OIDC_CLIENT_SECRET", cp)
                else:
                    self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", cp, "public clients must not read an ambient Secret key")
                for name in ["modelgateway-deployment.yaml", "console.yaml", "app-edge.yaml"]:
                    self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", section(text, name))
                if existing:
                    self.assertNotIn("kind: Secret", section(text, "secret.yaml"))
                elif secret:
                    self.assertIn(base64.b64encode(secret.encode()).decode(), section(text, "secret.yaml"))
                if secret:
                    rp = cp.split("- name: IDENTITY_OIDC_CLIENT_SECRET", 1)[1].split("readinessProbe:", 1)[0]
                    self.assertIn("optional: false", rp)
                    if existing:
                        self.assertIn('key: "confidential-rp"', rp)

    def test_trusted_proxy_and_gateway_off(self):
        values = native("trusted_proxy")
        values["identity"]["proxy"] = {"preset": "gcp-iap", "audience": "render-only-audience"}
        values["modelgateway"] = {"enabled": False}
        values["brain"] = {"modelProviders": [{"match": "*", "protocol": "anthropic", "model": "unused", "base_url": "https://unused.example"}]}
        text = render(values)
        self.assertFalse(section(text, "modelgateway-deployment.yaml"))
        self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", text)
        self.assertIn('value: ""', section(text, "app-edge.yaml"))

    def test_invalid_modes_origins_and_client_even_with_existing_secret(self):
        changes = [
            {"developmentMode": False},
            {"developmentMode": "false"},
            {"console": {"enabled": "false", "authMode": "platform", "publicOrigin": "http://localhost:3000"}},
            {"console": {"enabled": False, "authMode": "platform", "publicOrigin": "http://localhost:3000"}},
            {"console": {"enabled": True, "authMode": "platform", "publicOrigin": "http://localhost:3000", "legacySecret": "legacy-credential"}},
            {"console": {"enabled": True, "authMode": "platform", "publicOrigin": "http://localhost:3000", "image": "example/console:latest"}},
            {"appEdge": {"image": "caddy:2.10-alpine"}},
            {"identity": {"mode": "disabled"}},
            {"console": {"enabled": True, "authMode": "platform", "publicOrigin": ""}},
            {"console": {"enabled": True, "authMode": "wrong"}},
            {"console": {"enabled": True, "authMode": "", "publicOrigin": "https://console.example"}},
        ]
        for origin in ["https://UPPER.example", "https://console.example/", "https://console.example:443", "http://console.example", "https://user:password@console.example"]:
            changes.append({"console": {"enabled": True, "authMode": "platform", "publicOrigin": origin}})
        for change in changes:
            for existing in [False, True]:
                values = native()
                values.update(change)
                if existing:
                    values.update(existingSecret="operator-secret", postgresql={"enabled": False}, minio={"enabled": False}, openbao={"enabled": False})
                with self.subTest(change=change, existing=existing):
                    render(values, success=False)
        for oidc in [{"audience": "client"}, {"audience": "client", "clientId": "different"}, {"audience": "client", "clientId": "client", "clientSecret": 123}, {"audience": "client", "clientId": "client", "clientSecretKey": "rp-key"}]:
            values = native("oidc")
            values["identity"]["oidc"] = oidc
            render(values, success=False)

        values = native("oidc")
        values.update(existingSecret="operator-secret", postgresql={"enabled": False}, minio={"enabled": False}, openbao={"enabled": False})
        values["identity"]["oidc"] = {"audience": "client", "clientId": "client", "clientSecret": "render-only-secret"}
        self.assertIn("existingSecret writes no RP secret", render(values, success=False))

    def test_independent_image_and_release_qualified_resources(self):
        for release in ["one", "two", "a-long-release-name-that-exercises-the-fifty-character-limit"]:
            # Helm itself caps release names at 53 characters.
            release = release[:53]
            for overrides in [False, True]:
                with self.subTest(release=release, overrides=overrides):
                    values = native()
                    values["image"] = {"tag": "unrelated-platform-tag"}
                    if overrides:
                        values["console"]["image"] = "mirror.example/console@sha256:" + "a" * 64
                        values["appEdge"] = {"image": "mirror.example/edge@sha256:" + "b" * 64, "service": {"port": 18000}}
                        values["controlplane"] = {"service": {"port": 18080}}
                        values["modelgateway"] = {"service": {"port": 18090}}
                    text = render(values, release)
                    self.assertIn(values["console"].get("image", CONSOLE), section(text, "console.yaml"))
                    if overrides:
                        self.assertIn(values["appEdge"]["image"], section(text, "app-edge.yaml"))
                    services, deployments = {}, {}
                    for component, container_port in [("console", 3000), ("app-edge", 8080)]:
                        body = section(text, component + ".yaml")
                        self.assertNotIn("unrelated-platform-tag", body)
                        # Inspect only these chart objects, not labels elsewhere in the document.
                        for kind, objects in [("Service", services), ("Deployment", deployments)]:
                            matches = re.findall(r"(?ms)^kind: " + kind + r"\n(.*?)(?=^apiVersion:|\Z)", body)
                            self.assertEqual(len(matches), 1, (component, kind))
                            objects[component] = matches[0]
                            name = re.search(r"(?m)^  name: (\S+)$", matches[0])
                            self.assertIsNotNone(name, (component, kind))
                            fullname = (release + "-managed-agent-platform")[:50].rstrip("-")
                            self.assertEqual(name.group(1), fullname + "-" + component)
                        service, deployment = services[component], deployments[component]
                        expected = {"app.kubernetes.io/name": "managed-agent-platform",
                                    "app.kubernetes.io/instance": release,
                                    "app.kubernetes.io/component": component}
                        # Compare the Service selector, Deployment selector and actual Pod labels.
                        for resource, pattern in [
                            (service, r"(?m)^  selector:\n((?:    \S[^\n]*\n)+)"),
                            (deployment, r"(?m)^    matchLabels:\n((?:      \S[^\n]*\n)+)"),
                            (deployment, r"(?m)^      labels:\n((?:        \S[^\n]*\n)+)"),
                        ]:
                            labels = re.search(pattern, resource)
                            self.assertIsNotNone(labels, (component, pattern))
                            self.assertEqual(dict(line.strip().split(": ", 1) for line in labels.group(1).splitlines()), expected)
                        target = re.search(r"(?m)^      targetPort: (\S+)$", service)
                        self.assertIsNotNone(target, component)
                        ports = dict(re.findall(r"(?m)^            - name: (\S+)\n              containerPort: (\d+)$", deployment))
                        self.assertIn(target.group(1), ports, component)
                        self.assertEqual(ports[target.group(1)], str(container_port), component)
                        port = re.search(r"(?m)^      port: (\d+)$", service)
                        self.assertIsNotNone(port, component)
                        self.assertEqual(int(port.group(1)), 3000 if component == "console" else (18000 if overrides else 8080))
                    for component, variable in [("console", "CONSOLE_UPSTREAM"), ("controlplane", "CONTROLPLANE_UPSTREAM"), ("modelgateway", "MODELGATEWAY_UPSTREAM")]:
                        service = services["console"] if component == "console" else section(text, component + "-service.yaml")
                        name = re.search(r"(?m)^  name: (\S+)$", service)
                        port = re.search(r"(?m)^      port: (\d+)$", service)
                        self.assertIsNotNone(name, component)
                        self.assertIsNotNone(port, component)
                        self.assertIn("- name: " + variable + '\n              value: "' + name.group(1) + ":" + port.group(1) + '"', deployments["app-edge"])

    def test_bundled_casdoor_owns_native_rp_secret_and_callback(self):
        values = native("oidc")
        values["console"]["publicOrigin"] = "https://console.example"
        values["identity"]["oidc"] = {"issuer": "https://idp.example", "audience": "client", "clientId": "client"}
        values["casdoor"] = {"enabled": True, "adminPassword": "render-only-password", "console": {"clientId": "client", "clientSecret": "render-only-secret", "redirectURIs": ["https://console.example/api/auth/callback"]}, "ingress": {"host": "idp.example"}}
        base = json.loads(json.dumps(values))
        for existing in [False, True]:
            if existing:
                values.update(existingSecret="operator-secret", postgresql={"enabled": False}, minio={"enabled": False}, openbao={"enabled": False})
                values["casdoor"]["database"] = {"dataSourceName": "user=render password=render host=pg dbname=casdoor"}
            text = render(values)
            self.assertEqual(text, render(values), "seed and checksums must render deterministically")
            raw = section(text, "casdoor-init-data-secret.yaml")
            seed = json.loads(textwrap.dedent(raw.split("  init_data.json: |\n", 1)[1]))
            app = seed["applications"][0]
            self.assertEqual(app["clientId"], "client")
            self.assertEqual(app["clientSecret"], "render-only-secret")
            self.assertEqual(app["redirectUris"], ["https://console.example/api/auth/callback"])
            self.assertEqual([u["name"] for u in seed["users"]], ["admin"])
            self.assertIn('client-secret: "render-only-secret"', raw)
            cp = section(text, "controlplane-deployment.yaml")
            self.assertIn("key: client-secret", cp)
            checksum = re.search(r'checksum/oidc-client: "([a-f0-9]+)"', cp)
            self.assertIsNotNone(checksum, "seed client rotation must roll the CP RP")
            rotated = json.loads(json.dumps(values))
            rotated["casdoor"]["console"]["clientSecret"] = "rotated-render-only-secret"
            rotated_cp = section(render(rotated), "controlplane-deployment.yaml")
            self.assertNotIn(checksum.group(0), rotated_cp)
            self.assertNotIn("identity-oidc-client-secret", section(text, "secret.yaml"))
            self.assertIn("kind: NetworkPolicy", section(text, "casdoor-networkpolicy.yaml"))
            self.assertIn("-casdoor-deny", section(text, "casdoor-ingress.yaml"))
        for field, bad in [("clientId", "wrong"), ("clientSecret", "wrong")]:
            bad_values = json.loads(json.dumps(base))
            bad_values["identity"]["oidc"][field] = bad
            self.assertIn("client", render(bad_values, success=False))
        values["casdoor"]["console"]["redirectURIs"] = ["https://console.example/callback"]
        self.assertIn("/api/auth/callback", render(values, success=False))

    def test_seed_parity(self):
        self.assertEqual((ROOT / "deploy/compose/casdoor-init-data.json").read_bytes(),
                         (CHART / "files/casdoor-init-data.json").read_bytes())
        seed = json.loads((CHART / "files/casdoor-init-data.json").read_text())
        self.assertEqual(seed["applications"][0]["redirectUris"], ["http://localhost:3000/api/auth/callback"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
