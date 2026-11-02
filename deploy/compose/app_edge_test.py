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


if __name__ == "__main__":
    unittest.main(verbosity=2)
