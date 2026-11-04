#!/usr/bin/env python3
"""Exercise the deployed Caddy routing with owned, distinguishable upstreams."""
import http.client
import http.server
import json
import os
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent
IMAGE = "caddy:2.10-alpine@sha256:4c6e91c6ed0e2fa03efd5b44747b625fec79bc9cd06ac5235a779726618e530d"
EVIDENCE = Path(os.environ["APP_EDGE_EVIDENCE"]) if "APP_EDGE_EVIDENCE" in os.environ else Path(tempfile.mkdtemp(prefix="map-edge-test-"))
EVIDENCE.mkdir(parents=True, exist_ok=True)


def docker(*args, check=True):
    result = subprocess.run(["docker", *args], capture_output=True, text=True)
    if check and result.returncode:
        raise RuntimeError(result.stderr)
    return result


class EdgeRouting(unittest.TestCase):
    def test_deployed_routing(self):
        seen = []
        cancelled = threading.Event()
        servers, containers = [], []
        receipt = {"before": docker("ps", "-aq", "--no-trunc").stdout.split(), "containers": []}
        def record():
            (EVIDENCE / "routing-receipt.json").write_text(json.dumps(receipt, indent=2))
        record()
        try:
            for owner in ["console", "controlplane", "gateway"]:
                class Handler(http.server.BaseHTTPRequestHandler):
                    def do_GET(self):
                        seen.append((self.server.owner, self.path))
                        self.send_response(200)
                        self.send_header("Content-Type", "text/event-stream" if self.path.endswith("/stream") else "application/json")
                        self.send_header("X-Upstream", self.server.owner)
                        self.end_headers()
                        if self.path.endswith("/stream"):
                            try:
                                for n in range(400):
                                    self.wfile.write(f"data: {n}\n\n".encode())
                                    self.wfile.flush()
                                    time.sleep(0.05)
                            except (BrokenPipeError, ConnectionResetError):
                                cancelled.set()
                        else:
                            self.wfile.write(json.dumps({"owner": self.server.owner, "traceparent": self.headers.get("traceparent")}).encode())
                    def log_message(self, *_):
                        pass
                server = http.server.ThreadingHTTPServer(("0.0.0.0", 0), Handler)
                server.daemon_threads = True
                server.owner = owner
                servers.append(server)
                threading.Thread(target=server.serve_forever, daemon=True).start()
            upstream = {s.owner: f"host.docker.internal:{s.server_port}" for s in servers}
            receipt["listeners"] = [{"pid": os.getpid(), "port": s.server_port, "owner": s.owner} for s in servers]
            for mode, gateway in [("", True), ("platform", True), ("platform", False)]:
                name = f"edge-{mode or 'legacy'}-{gateway}"
                cidfile = EVIDENCE / (name + ".cid")
                if cidfile.exists():
                    raise AssertionError("use a fresh evidence directory; never reuse an old container receipt")
                args = ["create", "--cidfile", str(cidfile), "--add-host=host.docker.internal:host-gateway", "-p", "127.0.0.1::8080", "-v", f"{ROOT / 'app-edge'}:/etc/caddy:ro", "--entrypoint", "/bin/sh"]
                env = {"CONSOLE_AUTH_MODE": mode, "CONSOLE_PUBLIC_ORIGIN": "http://localhost:3000" if mode else "", "IDENTITY_MODE": "password" if mode else "", "CONTROLPLANE_UPSTREAM": upstream["controlplane"], "CONSOLE_UPSTREAM": upstream["console"], "MODELGATEWAY_UPSTREAM": upstream["gateway"] if gateway else ""}
                for key, value in env.items():
                    args.extend(["-e", f"{key}={value}"])
                args.extend([IMAGE, "/etc/caddy/start.sh"])
                entry = {"args": args, "cidfile": str(cidfile)}
                receipt["containers"].append(entry)
                record()
                try:
                    docker(*args)
                finally:
                    if cidfile.exists():
                        cid = cidfile.read_text().strip()
                        entry["id"] = cid
                        containers.append(cid)
                        entry["mounts"] = json.loads(docker("inspect", "--format", "{{json .Mounts}}", cid).stdout)
                        record()
                docker("start", cid)
                port = int(docker("port", cid, "8080/tcp").stdout.strip().rsplit(":", 1)[1])
                def request(path):
                    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
                    conn.request("GET", path, headers={"traceparent": "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"})
                    response = conn.getresponse()
                    status, owner, body = response.status, response.getheader("X-Upstream"), response.read()
                    conn.close()
                    return status, owner, body
                for _ in range(100):
                    try:
                        if request("/api/health")[0] == 200:
                            break
                    except (OSError, http.client.HTTPException):
                        pass
                    time.sleep(0.1)
                else:
                    raise AssertionError(docker("logs", cid).stdout + docker("logs", cid).stderr)
                routes = {"/login": "console", "/setup": "console", "/_next/static/test.js": "console", "/api/health": "console", "/v1/agents": "controlplane"}
                for path in ["/api/auth/config", "/api/auth/callback?code=test", "/api/console/organizations/default/members", "/api/oauth/test", "/api/organizations/default/proxy/v1/agents"]:
                    routes[path] = "controlplane" if mode else "console"
                for path in ["/api/login", "/api/platform/agents", "/api/gateway/providers"]:
                    routes[path] = "console"
                for path, expected in routes.items():
                    with self.subTest(mode=mode, gateway=gateway, path=path):
                        status, owner, body = request(path)
                        self.assertEqual((status, owner), (200, expected))
                        self.assertIn(b"0123456789abcdef0123456789abcdef", body)
                self.assertEqual(request("/admin/v1/providers")[:2], (200, "gateway") if mode and gateway else (503 if mode else 404, None))
                for path in ["/internal/v1/gate/config", "/%69nternal/v1/gate/config", "//internal/v1/gate/config", "/x/../internal/v1/gate/config", "/%2e%2e/internal/v1/gate/config", "/internal%2fv1/gate/config", "/%2569nternal/v1/gate/config", "/internal\\v1/gate/config"]:
                    before = len(seen)
                    with self.subTest(mode=mode, path=path):
                        self.assertEqual(request(path)[:2], (404, None))
                        self.assertEqual(len(seen), before, "refused path reached an upstream")
                if mode:
                    cancelled.clear()
                    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=3)
                    conn.request("GET", "/api/organizations/default/proxy/v1/sessions/test/events/stream")
                    response = conn.getresponse()
                    self.assertEqual(response.getheader("Content-Type"), "text/event-stream")
                    self.assertIsNone(response.getheader("Content-Encoding"))
                    self.assertEqual(response.readline(), b"data: 0\n")
                    response.close()
                    conn.close()
                    self.assertTrue(cancelled.wait(3), "Caddy did not propagate the original client disconnect")
                entry["logs"] = docker("logs", cid).stderr
                record()
        finally:
            errors = []
            for cid in containers:
                try:
                    docker("rm", "-f", "-v", cid)
                except Exception as error:
                    errors.append(str(error))
            for server in servers:
                server.shutdown()
                server.server_close()
            receipt["listenerObservations"] = [{"port": server.server_port, "closed": server.socket.fileno() == -1} for server in servers]
            receipt["after"] = docker("ps", "-aq", "--no-trunc").stdout.split()
            volumes = docker("volume", "ls", "-q").stdout.split()
            receipt["volumeObservations"] = [{"name": mount["Name"], "absent": mount["Name"] not in volumes}
                                             for entry in receipt["containers"] for mount in entry.get("mounts", [])
                                             if mount["Type"] == "volume"]
            receipt["cleanupErrors"] = errors
            receipt["cleaned"] = (not errors and all(cid not in receipt["after"] for cid in containers)
                                  and all(item["absent"] for item in receipt["volumeObservations"]))
            receipt["seen"] = seen
            record()
            self.assertTrue(receipt["cleaned"], "owned Caddy cleanup was not observed")


class AliasNameRouting(unittest.TestCase):
    """Alias data may be escaped; namespace/traversal spellings must not dispatch."""

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="alias-routing-", dir=EVIDENCE))
        self.receipt = {"pid": os.getpid(), "pgid": os.getpgid(0), "containers": [], "commands": []}
        self.network = None
        self.forwarded = []
        self.refused = []
        self.addCleanup(self.cleanup)
        self.network = self.command("network", "create", self.directory.name).stdout.strip()
        self.receipt["network"] = self.network
        self.receipt["networkConfig"] = json.loads(self.command("network", "inspect", self.network).stdout)[0]
        config = self.directory / "origin.caddy"
        config.write_text("{\n admin off\n auto_https off\n}\n" + "\n".join(
            f':{port} {{\n log {{\n output stdout\n format json\n }}\n'
            f' header X-Upstream {owner}\n respond "{{http.request.method}} {{http.request.uri}}" 200\n}}'
            for port, owner in [(8101, "console"), (8102, "controlplane"), (8103, "gateway")]))
        self.origin = self.create("origin", ["--network-alias", "origin", "-v", f"{config}:/etc/caddy/Caddyfile:ro"],
                                  ["caddy", "run", "--config", "/etc/caddy/Caddyfile"])
        inspected = json.loads(self.command("inspect", self.origin).stdout)[0]
        self.receipt["originPortBindings"] = inspected["HostConfig"]["PortBindings"]
        self.assertFalse(self.receipt["originPortBindings"], "origin must have no published backend port")
        self.record()

    def record(self):
        (self.directory / "receipt.json").write_text(json.dumps(self.receipt, indent=2))

    def command(self, *args, check=True):
        process = subprocess.Popen(["docker", *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                   text=True, start_new_session=True)
        entry = {"args": list(args), "pid": process.pid, "pgid": process.pid}
        self.receipt["commands"].append(entry)
        self.record()
        stdout, stderr = process.communicate()
        entry["returncode"] = process.returncode
        self.record()
        if check and process.returncode:
            raise AssertionError(stderr or stdout)
        return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def create(self, name, args, command):
        cidfile = self.directory / (name + ".cid")
        entry = {"name": name, "cidfile": str(cidfile)}
        self.receipt["containers"].append(entry)
        try:
            self.command("create", "--pull=never", "--cidfile", str(cidfile), "--network", self.network,
                         *args, "--entrypoint", command[0], IMAGE, *command[1:])
        finally:
            if cidfile.exists():
                entry["id"] = cidfile.read_text().strip()
                entry["mounts"] = json.loads(self.command("inspect", "--format", "{{json .Mounts}}", entry["id"]).stdout)
                self.record()
        self.command("start", entry["id"])
        entry["state"] = json.loads(self.command("inspect", "--format", "{{json .State}}", entry["id"]).stdout)
        logs = self.command("logs", entry["id"])
        entry["logs"] = logs.stdout + logs.stderr
        self.record()
        self.assertTrue(entry["state"]["Running"], entry["logs"])
        return entry["id"]

    def request(self, port, method, path):
        case = f"case-{len(self.receipt.get('requests', []))}"
        conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
        try:
            conn.request(method, path, headers={"X-Edge-Case": case})
            response = conn.getresponse()
            result = response.status, response.getheader("X-Upstream"), response.read().decode()
        finally:
            conn.close()
        self.receipt.setdefault("requests", []).append({"case": case, "method": method, "path": path,
                                                        "status": result[0], "owner": result[1], "body": result[2]})
        self.record()
        return case, result

    def test_alias_data_and_namespace_denial(self):
        # These literals follow checkAliasName and the Console gateway BFF receiver.
        # The encoded climb is *data* only when the percent itself is escaped.
        names = ["fast", "Qwen/Qwen3-Coder", "literal%252Fsegment", "literal/segment",
                 "%252e%252e", "a/%252e%252e/b", "a%25b", "a%5Cb", "a%3Fb%3Dc",
                 "a%23b", "model%2Ev1", "%E6%A8%A1%E5%9E%8B"]
        internal = ["/internal/v1/gate/config", "/%69nternal/v1/gate/config", "//internal/v1/gate/config",
                    "/x/../internal/v1/gate/config", "/%2e%2e/internal/v1/gate/config",
                    "/internal%2fv1/gate/config", "/%2569nternal/v1/gate/config", "/internal\\v1/gate/config"]
        prefixes = ["/admin%2fv1/aliases/a", "/admin/v1%2faliases/a", "/admin/v1/aliases%2fa",
                    "/%61dmin/v1/aliases/a%25b", "/admin/v1/%61liases/a%25b",
                    "/admin/v1/aliases-extra/a%25b", "/api%2fgateway/admin/v1/aliases/a",
                    "/api/gateway%2fadmin/v1/aliases/a", "/api/gateway/admin/v1/aliases%2fa",
                    "/api/gateway/admin/v1/aliases-extra/a%25b", "/api/%2567ateway/admin/v1/aliases/a",
                    "/admin//v1/aliases/a", "/admin/v1/./aliases/a", "/admin\\v1/aliases/a"]
        other = ["/admin/v1/providers/a%25b", "/api/gateway/admin/v1/providers/a%5Cb",
                 "/v1/agents/a%2Fb", "/api/auth/a%252e", "/api/console/a%2Eb",
                 "/api/organizations/default/proxy/v1/agents/a%25b"]
        invalid_names = ["../providers", "a/../../providers", "%2e", ".%2E", "%2e%2E/providers",
                         "a%2f..%2F..%2fproviders", "a/%2E%2e/providers", "a//b", "%2fa",
                         "a/%2fb", "a%2F%2fb", "a%2f/b", "a%2f?query=%25", "a\\b"]
        sources = [("compose", ROOT / "app-edge"),
                   ("helm", ROOT.parent / "helm/managed-agent-platform/files/app-edge")]
        for source, directory in sources:
            for mode, gateway in [("", True), ("platform", True), ("platform", False)]:
                label = f"{source}-{mode or 'legacy'}-{gateway}"
                args = ["-p", "127.0.0.1::8080", "-v", f"{directory}:/etc/caddy:ro"]
                env = {"CONSOLE_AUTH_MODE": mode, "CONSOLE_PUBLIC_ORIGIN": "http://localhost:3000" if mode else "",
                       "IDENTITY_MODE": "password" if mode else "", "CONTROLPLANE_UPSTREAM": "origin:8102",
                       "CONSOLE_UPSTREAM": "origin:8101", "MODELGATEWAY_UPSTREAM": "origin:8103" if gateway else ""}
                for key, value in env.items():
                    args.extend(["-e", f"{key}={value}"])
                cid = self.create(label, args, ["/bin/sh", "/etc/caddy/start.sh"])
                bindings = json.loads(self.command("inspect", "--format", "{{json .NetworkSettings.Ports}}", cid).stdout)
                bindings = {key: value for key, value in bindings.items() if value}
                self.receipt["containers"][-1]["publishedPorts"] = bindings
                self.record()
                self.assertEqual(set(bindings), {"8080/tcp"})
                self.assertEqual(len(bindings["8080/tcp"]), 1)
                self.assertEqual(bindings["8080/tcp"][0]["HostIp"], "127.0.0.1")
                port = int(bindings["8080/tcp"][0]["HostPort"])
                for _ in range(100):
                    try:
                        case, result = self.request(port, "GET", "/api/health")
                        if result[0] == 200:
                            self.forwarded.append(case)
                            break
                    except (OSError, http.client.HTTPException):
                        pass
                    time.sleep(0.1)
                else:
                    self.fail(self.command("logs", cid).stderr)
                for prefix in ["/admin/v1/aliases/", "/api/gateway/admin/v1/aliases/"]:
                    paths = names + (["Qwen%2FQwen3-Coder"] if prefix.startswith("/admin/") else [])
                    for name in paths:
                        for method in ["GET", "POST", "DELETE"]:
                            path = prefix + name + "?query=%252f"
                            case, result = self.request(port, method, path)
                            enabled = prefix.startswith("/api/") or (mode and gateway)
                            if enabled:
                                self.forwarded.append(case)
                            else:
                                self.refused.append(case)
                            with self.subTest(source=source, mode=mode, gateway=gateway, method=method, path=path):
                                expected = (200, "console" if prefix.startswith("/api/") else "gateway", method + " " + path)
                                if enabled:
                                    self.assertEqual(result, expected, "escaped alias data must arrive byte-for-byte")
                                else:
                                    self.assertEqual(result[:2], (503 if mode else 404, None))
                denied = internal + prefixes + other + [prefix + name for prefix in
                         ["/admin/v1/aliases/", "/api/gateway/admin/v1/aliases/"] for name in invalid_names]
                for path in denied:
                    for method in ["GET", "POST", "DELETE"]:
                        case, result = self.request(port, method, path)
                        self.refused.append(case)
                        with self.subTest(source=source, mode=mode, gateway=gateway, method=method, denied=path):
                            self.assertEqual(result[:2], (404, None))
        logs = self.command("logs", self.origin)
        (self.directory / "origin.log").write_text(logs.stdout + logs.stderr)
        seen = []
        for line in logs.stdout.splitlines():
            entry = json.loads(line)
            seen.extend(entry.get("request", {}).get("headers", {}).get("X-Edge-Case", []))
        self.receipt["seen"] = seen
        self.receipt["expectedForwarded"] = self.forwarded
        self.receipt["expectedRefused"] = self.refused
        self.record()
        self.assertCountEqual(seen, self.forwarded, "refused paths must have zero upstream dispatch")
        self.assertTrue(set(seen).isdisjoint(self.refused))

    def cleanup(self):
        errors = []
        for entry in self.receipt["containers"]:
            if "id" in entry:
                result = self.command("rm", "-f", "-v", entry["id"], check=False)
                if result.returncode:
                    errors.append(result.stderr)
        if self.network:
            result = self.command("network", "rm", self.network, check=False)
            if result.returncode:
                errors.append(result.stderr)
        after = self.command("ps", "-aq", "--no-trunc").stdout.split()
        volumes = self.command("volume", "ls", "-q").stdout.split()
        networks = self.command("network", "ls", "--no-trunc", "-q").stdout.split()
        self.receipt["cleanupErrors"] = errors
        self.receipt["containerRemovalObserved"] = all(e["id"] not in after for e in self.receipt["containers"] if "id" in e)
        self.receipt["volumeRemovalObserved"] = all(m["Name"] not in volumes for e in self.receipt["containers"]
                                                   for m in e.get("mounts", []) if m["Type"] == "volume")
        self.receipt["networkRemovalObserved"] = self.network not in networks
        self.record()
        self.assertFalse(errors)
        self.assertTrue(self.receipt["containerRemovalObserved"])
        self.assertTrue(self.receipt["volumeRemovalObserved"])
        self.assertTrue(self.receipt["networkRemovalObserved"])


if __name__ == "__main__":
    unittest.main(verbosity=2)
