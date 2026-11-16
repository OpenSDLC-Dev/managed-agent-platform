#!/usr/bin/env python3
"""Credential-free one-origin contracts, including the pinned edge executable."""
import base64
import json
import re
import signal
import subprocess
import tempfile
import textwrap
import unittest
import uuid
from pathlib import Path

CHART = Path(__file__).resolve().parents[1]
ROOT = CHART.parents[2]
CONSOLE = "ghcr.io/opensdlc-dev/managed-agent-console@sha256:48b137660daca7214f012357051a7f89f7da579f62acb604992c98a7c70ff319"


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
    def test_edge_binary_executes_with_rendered_capabilities(self):
        body = section(render(native()), "app-edge.yaml")
        container = body.split("        - name: app-edge\n", 1)[1]
        image = json.loads(re.search(r"(?m)^          image: (.+)$", container).group(1))
        context = re.search(r"(?ms)^          securityContext:\n(.*?)(?=^          \S|^      \S|\Z)", container).group(1)
        # Consume the chart's rendered settings: a hard-coded Docker capability
        # would pass even when a chart regression makes the image unexecutable.
        self.assertRegex(context, r"(?m)^            allowPrivilegeEscalation: false$")
        args = ["--security-opt=no-new-privileges"]
        for action in ["drop", "add"]:
            match = re.search(r"(?m)^              " + action + r": \[([A-Z_, ]+)\]$", context)
            if match:
                caps = [cap.strip() for cap in match.group(1).split(",")]
                self.assertEqual(caps, ["ALL"] if action == "drop" else ["NET_BIND_SERVICE"])
                args.extend("--cap-" + action + "=" + cap for cap in caps)
        self.assertIn("--cap-drop=ALL", args)
        # Bound registry access before there is any container to own. A cached
        # pinned image needs no network, and create must not pull implicitly.
        cached = subprocess.run(["docker", "image", "inspect", image], capture_output=True, text=True, timeout=30)
        if cached.returncode:
            pulled = subprocess.run(["docker", "pull", image], capture_output=True, text=True, timeout=120)
            self.assertEqual(pulled.returncode, 0, pulled.stdout + pulled.stderr)
        interrupted = []
        previous = {sig: signal.signal(sig, lambda number, _: interrupted.append(number))
                    for sig in [signal.SIGINT, signal.SIGTERM]}
        try:
            with tempfile.TemporaryDirectory(prefix="helm-edge-exec-") as directory:
                cidfile = Path(directory) / "container.cid"
                owner = uuid.uuid4().hex
                name = "helm-edge-exec-" + owner
                label = "io.opensdlc.test.helm-edge=" + owner
                try:
                    # Killing the CLI does not cancel daemon-side creation. Wait
                    # for this original request, outside the terminal's signal
                    # group, before looking up and removing its owned container.
                    created = subprocess.run(["docker", "create", "--pull=never", "--name", name, "--label", label,
                                              "--cidfile", str(cidfile), *args,
                                              "--entrypoint", "/bin/sh", image, "-c", "exec caddy version"],
                                             capture_output=True, text=True, start_new_session=True)
                    self.assertEqual(created.returncode, 0, created.stdout + created.stderr)
                    cid = cidfile.read_text().strip()
                    if not interrupted:
                        result = subprocess.run(["docker", "start", "-a", cid], capture_output=True, text=True,
                                                timeout=30, start_new_session=True)
                        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
                        self.assertRegex(result.stdout, r"^v\d+\.\d+\.\d+", result.stdout + result.stderr)
                finally:
                    # The name and nonce identify our original request even if
                    # Docker failed to write its CID file after creation.
                    owned = subprocess.run(["docker", "ps", "-aq", "--no-trunc",
                                            "--filter", "name=^/" + name + "$", "--filter", "label=" + label],
                                           capture_output=True, text=True, timeout=30, start_new_session=True)
                    self.assertEqual(owned.returncode, 0, owned.stdout + owned.stderr)
                    for cid in owned.stdout.split():
                        removed = subprocess.run(["docker", "rm", "-f", "-v", cid],
                                                 capture_output=True, text=True, timeout=30, start_new_session=True)
                        self.assertEqual(removed.returncode, 0, removed.stdout + removed.stderr)
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
            if interrupted:
                signal.raise_signal(interrupted[0])

    def test_application_service_accepts_gke_backend_annotations(self):
        values = native()
        values["appEdge"] = {"service": {"annotations": {
            "cloud.google.com/neg": '{"ingress":true}',
            "cloud.google.com/backend-config": '{"default":"map-native-edge"}',
        }}}
        body = section(render(values), "app-edge.yaml")
        service = re.search(r"(?ms)^kind: Service\n(.*?)(?=^apiVersion:|\Z)", body).group(1)
        metadata = service.split("\nspec:", 1)[0]
        self.assertIn("app.kubernetes.io/component: app-edge", metadata)
        self.assertIn('cloud.google.com/neg:', service)
        self.assertIn('cloud.google.com/backend-config:', service)
        self.assertIn('map-native-edge', service)

    def test_requires_login_even_without_console_or_with_external_secret(self):
        for mode in ["", "disabled", "oidc"]:
            for existing in [False, True]:
                values = {"identity": {"mode": mode}, "console": {"enabled": False, "authMode": "", "publicOrigin": ""}}
                if existing:
                    values.update(existingSecret="operator-secret", postgresql={"enabled": False}, minio={"enabled": False}, openbao={"enabled": False})
                with self.subTest(mode=mode, existing=existing):
                    render(values, success=False)

    def test_example_uses_native_login(self):
        default = render()
        self.assertIn("CONSOLE_AUTH_MODE", default)
        self.assertIn(CONSOLE, section(default, "console.yaml"))
        self.assertNotIn("controlplane-api-key", section(default, "console.yaml"))
        self.assertNotIn("IDENTITY_OIDC_CLIENT_SECRET", default)

    def test_retained_server_keys_do_not_reach_native_console(self):
        def pods(values):
            text = render(values)
            return {name: section(text, name).split("\n  template:\n", 1)[1]
                    for name in ["console.yaml", "controlplane-deployment.yaml", "modelgateway-deployment.yaml"]}

        for mode in ["managed", "existing"]:
            with self.subTest(mode=mode):
                values = native()
                if mode == "existing":
                    values.update(existingSecret="external-platform", postgresql={"enabled": False},
                                  minio={"enabled": False}, openbao={"enabled": False})
                values["controlplane"] = {"apiKey": "render-only-old-key"}
                old = pods(values)
                values["controlplane"]["apiKey"] = "render-only-new-key"
                new = pods(values)
                self.assertEqual(old["console.yaml"], new["console.yaml"])
                for name in ["controlplane-deployment.yaml", "modelgateway-deployment.yaml"]:
                    self.assertEqual(old[name] != new[name], mode != "existing")
                self.assertNotIn("checksum/", new["console.yaml"])
                self.assertNotIn("controlplane-api-key", new["console.yaml"])
                if mode == "existing":
                    values["existingSecret"] = "external-platform-next"
                    renamed = pods(values)
                    self.assertEqual(new["console.yaml"], renamed["console.yaml"])
                    self.assertNotIn("checksum/", renamed["console.yaml"])

    def test_console_numeric_non_root_user(self):
        for mode, values in [("password", native()), ("oidc", native("oidc"))]:
            if mode == "oidc":
                values["identity"]["oidc"] = {"issuer": "https://idp.example", "audience": "client", "clientId": "client"}
            with self.subTest(mode=mode):
                body = section(render(values), "console.yaml")
                deployments = re.findall(r"(?ms)^kind: Deployment\n(.*?)(?=^apiVersion:|\Z)", body)
                self.assertEqual(len(deployments), 1)
                containers = re.findall(r"(?ms)^        - name: console\n(.*?)(?=^        - name:|\Z)", deployments[0])
                self.assertEqual(len(containers), 1)
                contexts = re.findall(r"(?ms)^          securityContext:\n(.*?)(?=^          \S|\Z)", containers[0])
                self.assertEqual(len(contexts), 1)
                context = contexts[0]
                self.assertRegex(context, r"(?m)^            runAsUser: 1000$",
                                 "Console container must use numeric UID 1000")
                self.assertRegex(context, r"(?m)^            runAsGroup: 1000$",
                                 "Console container must use numeric GID 1000")
                self.assertRegex(context, r"(?m)^            runAsNonRoot: true$")
                self.assertRegex(context, r"(?m)^            allowPrivilegeEscalation: false$")
                self.assertRegex(context, r"(?m)^            capabilities:\n              drop: \[ALL\]$")

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
