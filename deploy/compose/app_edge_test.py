#!/usr/bin/env python3
"""Exercise the deployed Caddy routing with owned, distinguishable upstreams."""
import http.client
import http.server
import json
import os
import signal
import subprocess
import sys
import tempfile
import threading
import time
import types
import unittest
from contextlib import contextmanager
from pathlib import Path
import unittest.mock as mock

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
                        # Startup errors are retried within the bounded readiness loop.
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

    def run(self, result=None):
        self.pending_interrupt = False
        self.cleaning = False
        try:
            return super().run(result)
        finally:
            # unittest skips doCleanups when setup or the test raises KeyboardInterrupt.
            # Already-run cleanups have been popped, so ordinary failures do not repeat them.
            with self.defer_interrupts():
                self.doCleanups()
            self.raise_interrupt()

    @contextmanager
    def defer_interrupts(self):
        def interrupted(*_):
            self.pending_interrupt = True
        previous = signal.signal(signal.SIGINT, interrupted)
        try:
            yield
        finally:
            signal.signal(signal.SIGINT, previous)

    def raise_interrupt(self):
        if self.pending_interrupt and not self.cleaning:
            self.pending_interrupt = False
            raise KeyboardInterrupt("after owned command completion and ownership capture")

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="alias-routing-", dir=EVIDENCE))
        self.receipt = {"pid": os.getpid(), "pgid": os.getpgid(0), "containers": [], "commands": [], "requests": []}
        self.network = None
        self.active = {}
        self.forwarded = []
        self.refused = []
        self.addCleanup(self.cleanup)
        self.command("pull", IMAGE)
        self.command("network", "create", self.directory.name, acquire=self.capture_network)
        self.assertTrue(self.network, "original network creation returned no ownership receipt")
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

    def record(self, final=False):
        # Only ownership is checkpointed. Histories are written once, after cleanup.
        value = self.receipt if final else {
            "pid": self.receipt["pid"], "pgid": self.receipt["pgid"], "network": self.network,
            "containers": [{key: entry[key] for key in ("name", "cidfile", "id", "mounts") if key in entry}
                           for entry in self.receipt["containers"]]}
        (self.directory / "receipt.json").write_text(json.dumps(value, indent=2))

    def capture_network(self, stdout):
        if stdout.strip():
            self.network = stdout.strip()
            self.receipt["network"] = self.network
            self.record()
        return bool(self.network)

    def communicate(self, process, timeout):
        deadline = time.monotonic() + timeout
        try:
            return process.communicate(timeout=timeout)
        except KeyboardInterrupt:
            self.pending_interrupt = True
        # Finish the same exchange, not a replacement create or a name lookup.
        with self.defer_interrupts():
            while time.monotonic() < deadline:
                try:
                    return process.communicate(timeout=max(.001, deadline - time.monotonic()))
                except KeyboardInterrupt:
                    self.pending_interrupt = True
        raise subprocess.TimeoutExpired(process.args, timeout)

    def child_group_alive(self, process, entry):
        pgid = entry["pgid"]
        # start_new_session owns this original independent group, not just its
        # leader. Never signal the parent group or a replacement looked up by name.
        if pgid <= 0 or pgid != process.pid or pgid == os.getpgid(0):
            raise RuntimeError("owned child lacks an independent original process group")
        try:
            os.killpg(pgid, 0)
        except ProcessLookupError:
            return False
        return True

    def stop_child(self, process, entry):
        for sig in (signal.SIGTERM, signal.SIGKILL):
            deadline = time.monotonic() + 5
            if self.child_group_alive(process, entry):
                try:
                    os.killpg(entry["pgid"], sig)
                except ProcessLookupError:
                    # The group can exit after the liveness probe.
                    pass
            try:
                output = self.communicate(process, max(.001, deadline - time.monotonic()))
            except subprocess.TimeoutExpired:
                continue
            while self.child_group_alive(process, entry):
                if time.monotonic() >= deadline:
                    break
                time.sleep(.01)
            else:
                if process.returncode is not None:
                    return output
        raise RuntimeError(f"owned child {process.pid} could not be drained, reaped and group-quiesced")

    def settle_child(self, process, entry, acquire, output=None):
        if acquire:
            entry["ownershipUnresolved"] = True
        try:
            if output is None or self.child_group_alive(process, entry):
                output = self.stop_child(process, entry)
            if process.returncode is None:
                raise RuntimeError(f"owned child {process.pid} was not reaped")
        except Exception as error:
            entry["reapError"] = str(error)
            entry["returncode"] = process.returncode
            return output
        entry["returncode"] = process.returncode
        self.active.pop(process.pid, None)
        if acquire:
            try:
                entry["ownershipUnresolved"] = not acquire(output[0] if output else "")
            except Exception as error:
                entry["ownershipError"] = repr(error)
                raise
        return output

    def command(self, *args, check=True, acquire=None):
        process, entry, output = None, None, None
        deadline = time.monotonic() + 30
        try:
            try:
                # Include the spawn-to-communicate boundary in the interrupt guard.
                with self.defer_interrupts():
                    process = subprocess.Popen(["docker", *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                                               text=True, start_new_session=True)
                    entry = {"args": list(args), "pid": process.pid, "pgid": process.pid}
                    self.receipt["commands"].append(entry)
                    self.active[process.pid] = process, entry, acquire
                output = self.communicate(process, max(.001, deadline - time.monotonic()))
            except KeyboardInterrupt:
                self.pending_interrupt = True
                if process is None:
                    raise
                with self.defer_interrupts():
                    output = self.communicate(process, max(.001, deadline - time.monotonic()))
        except BaseException as error:
            if entry is not None:
                entry["error"] = repr(error)
            raise
        finally:
            if process is not None:
                with self.defer_interrupts():
                    output = self.settle_child(process, entry, acquire, output)
        self.raise_interrupt()
        if process.pid in self.active:
            raise RuntimeError(f"owned child {process.pid} still active after command settlement")
        stdout, stderr = output
        if check and process.returncode:
            raise AssertionError(stderr or stdout)
        return subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)

    def create(self, name, args, command):
        cidfile = self.directory / (name + ".cid")
        self.assertFalse(cidfile.exists(), "use a fresh owned cidfile")
        entry = {"name": name, "cidfile": str(cidfile)}
        self.receipt["containers"].append(entry)

        def capture(_):
            if cidfile.exists():
                cid = cidfile.read_text().strip()
                if cid:
                    entry["id"] = cid
                    self.record()
            return "id" in entry

        self.command("create", "--pull=never", "--cidfile", str(cidfile), "--network", self.network,
                     *args, "--entrypoint", command[0], IMAGE, *command[1:], acquire=capture)
        self.assertIn("id", entry, "original container creation returned no ownership receipt")
        entry["mounts"] = json.loads(self.command("inspect", "--format", "{{json .Mounts}}", entry["id"]).stdout)
        self.record()
        self.command("start", entry["id"])
        entry["state"] = json.loads(self.command("inspect", "--format", "{{json .State}}", entry["id"]).stdout)
        logs = self.command("logs", entry["id"])
        entry["logs"] = logs.stdout + logs.stderr
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
                        # Startup errors are retried within the bounded readiness loop.
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
        self.assertCountEqual(seen, self.forwarded, "refused paths must have zero upstream dispatch")
        self.assertTrue(set(seen).isdisjoint(self.refused))

    def cleanup(self):
        errors = []
        self.receipt["cleanupErrors"] = errors
        self.receipt.update(containerRemovalObserved=False, volumeRemovalObserved=False, networkRemovalObserved=False)

        def attempt(*args):
            if self.active and (args[0] == "rm" or args[:2] == ("network", "rm")):
                errors.append("owned children still active; resource cleanup withheld")
                return None
            try:
                result = self.command(*args, check=False)
                if not result.returncode:
                    return result
                errors.append(result.stderr or result.stdout or repr(args))
            except Exception as error:
                errors.append(f"{args!r}: {error}")
            return None

        with self.defer_interrupts():
            self.cleaning = True
            try:
                for process, entry, acquire in list(self.active.values()):
                    try:
                        self.settle_child(process, entry, acquire)
                    except Exception as error:
                        errors.append(f"owned child {process.pid}: {error}")
                unresolved = [entry for entry in self.receipt["commands"] if entry.get("ownershipUnresolved")]
                if unresolved:
                    errors.append("original creation ownership unresolved; network cleanup withheld")
                if self.active:
                    errors.append("owned children still active; resource cleanup withheld")
                else:
                    if getattr(self, "origin", None) and "seen" not in self.receipt:
                        logs = attempt("logs", self.origin)
                        if logs is not None:
                            try:
                                (self.directory / "origin.log").write_text(logs.stdout + logs.stderr)
                                self.receipt["seen"] = [case for line in logs.stdout.splitlines()
                                                        for case in json.loads(line).get("request", {}).get("headers", {}).get("X-Edge-Case", [])]
                            except Exception as error:
                                errors.append(f"partial origin log: {error}")
                    for entry in self.receipt["containers"]:
                        if "id" not in entry:
                            continue
                        if "mounts" not in entry:
                            mounts = attempt("inspect", "--format", "{{json .Mounts}}", entry["id"])
                            if mounts is not None:
                                try:
                                    entry["mounts"] = json.loads(mounts.stdout)
                                    self.record()
                                except Exception as error:
                                    errors.append(f"owned mount receipt: {error}")
                        attempt("rm", "-f", "-v", entry["id"])
                    if self.network and not unresolved:
                        attempt("network", "rm", self.network)
                after = attempt("ps", "-aq", "--no-trunc")
                volumes = attempt("volume", "ls", "-q")
                networks = attempt("network", "ls", "--no-trunc", "-q")
                container_ids = set(after.stdout.split()) if after is not None else None
                volume_names = set(volumes.stdout.split()) if volumes is not None else None
                network_ids = set(networks.stdout.split()) if networks is not None else None
                known = not unresolved and not self.active
                self.receipt["containerRemovalObserved"] = bool(after is not None and known and all(
                    e["id"] not in container_ids for e in self.receipt["containers"] if "id" in e))
                self.receipt["volumeRemovalObserved"] = bool(volumes is not None and known and all(
                    "mounts" in e for e in self.receipt["containers"] if "id" in e) and all(
                    m["Name"] not in volume_names for e in self.receipt["containers"]
                    for m in e.get("mounts", []) if m["Type"] == "volume"))
                self.receipt["networkRemovalObserved"] = bool(networks is not None and known and self.network not in network_ids)
            except Exception as error:
                errors.append(str(error))
            finally:
                self.receipt.setdefault("seen", [])
                self.receipt["expectedForwarded"] = self.forwarded
                self.receipt["expectedRefused"] = self.refused
                try:
                    self.record(final=True)
                finally:
                    self.cleaning = False
        self.assertFalse(errors)
        self.assertTrue(self.receipt["containerRemovalObserved"])
        self.assertTrue(self.receipt["volumeRemovalObserved"])
        self.assertTrue(self.receipt["networkRemovalObserved"])


class AliasFixtureContracts(unittest.TestCase):
    """Exercise the real fixture lifecycle, never Docker or Caddy routing."""

    class Process:
        def __init__(self, transport, args):
            self.transport, self.args = transport, list(args)
            self.pid = 800000 + len(transport.processes)
            self.returncode = None
            self.output = None
            self.group_alive, self.helper_live, self.group_stop_error = True, False, False

        def communicate(self, input=None, timeout=None):
            args, transport = self.args[1:], self.transport
            delayed = ((transport.fault == "network-response" and args[:2] == ["network", "create"])
                       or (transport.fault == "cidfile" and args[0] == "create"))
            if delayed:
                transport.fault = None
                # The network already exists but its original response is pending.
                # A container's cidfile is deliberately not available until retry.
                if args[0] == "network":
                    self.output = transport.execute(args, self)
                raise KeyboardInterrupt("delayed original creation response")
            if self.output is None:
                self.output = transport.execute(args, self)
            stdout, stderr, self.returncode = self.output
            if args[0] == "create" and transport.fault in ("helper-pipe", "helper-stop-error"):
                self.helper_live = True
                self.group_stop_error = transport.fault == "helper-stop-error"
                transport.fault = None
            self.group_alive = self.helper_live
            if self.helper_live:
                # The leader exited; a same-group helper still owns an output pipe.
                raise subprocess.TimeoutExpired(self.args, timeout)
            if transport.fault == "setup" and args[:2] == ["network", "inspect"]:
                transport.fault = None
                raise KeyboardInterrupt("setup after original network receipt")
            if transport.cleanup_exception and args[0] == "rm":
                transport.cleanup_exception = False
                raise OSError("fake cleanup transport failed")
            return stdout, stderr

        def poll(self):
            return self.returncode

        def wait(self, timeout=None):
            self.communicate(timeout=timeout)
            return self.returncode

        def terminate(self):
            self.returncode = -signal.SIGTERM

        def kill(self):
            self.returncode = -signal.SIGKILL

        def send_signal(self, sig):
            self.returncode = -sig

    class Transport:
        def __init__(self, fixture, fault=None, cleanup_exception=False):
            self.fixture, self.fault = fixture, fault
            self.cleanup_exception = cleanup_exception
            self.calls, self.processes, self.cleanup_observations = [], [], []
            self.ownership_errors = []
            self.images = set()
            self.networks = {"unrelated-network"}
            self.volumes = {"unrelated-volume"}
            self.containers = {"unrelated-container": {}}
            self.ports, self.seen = {}, []
            self.completed_requests = 0
            self.child_factory = None
            self.group_calls, self.real_groups = [], {}
            self.real_killpg, self.parent_pgid = os.killpg, os.getpgid(0)

        def killpg(self, pgid, sig):
            # Fabricated PIDs never reach the host. The existing synthetic child
            # is admitted only after its actual independent identity is recorded.
            if pgid <= 0 or pgid == self.parent_pgid or sig not in (0, signal.SIGTERM, signal.SIGKILL):
                raise AssertionError("unowned process-group operation")
            process = next((item for item in self.processes if item.pid == pgid), None)
            if process is None:
                raise AssertionError("no fallback for an unknown process group")
            self.group_calls.append({"pgid": pgid, "signal": sig})
            if isinstance(process, AliasFixtureContracts.Process):
                if not process.group_alive:
                    raise ProcessLookupError(pgid)
                if sig:
                    if process.group_stop_error:
                        raise OSError("fake owned group could not be signalled")
                    process.helper_live = process.group_alive = False
                    if process.returncode is None:
                        process.returncode = -sig
                return None
            if self.real_groups.get(pgid) != {"pid": process.pid, "pgid": pgid}:
                raise AssertionError("synthetic group lacks original identity evidence")
            return self.real_killpg(pgid, sig)

        def popen(self, args, **kwargs):
            if args[0] != "docker" or not kwargs.get("start_new_session"):
                raise AssertionError("only explicit fake detached Docker commands are admitted")
            self.calls.append(list(args[1:]))
            process = self.child_factory(args, kwargs) if self.child_factory else None
            if process is None:
                process = AliasFixtureContracts.Process(self, args)
            self.processes.append(process)
            return process

        def forbidden(self, *args, **kwargs):
            raise AssertionError("no fallback to a real command or HTTP transport")

        def persisted(self):
            path = self.fixture.directory / "receipt.json"
            return json.loads(path.read_text()) if path.exists() else {}

        def execute(self, args, process):
            if args == ["pull", IMAGE]:
                if self.fault == "image-preparation":
                    return "", "fake pinned image preparation failed", 1
                self.images.add(IMAGE)
                return IMAGE + "\n", "", 0
            if args[:2] == ["network", "create"]:
                self.networks.add("original-network-id")
                return "original-network-id\n", "", 0
            if args[:2] == ["network", "inspect"]:
                if self.persisted().get("network") != "original-network-id":
                    self.ownership_errors.append("network ownership not durable before inspection")
                return json.dumps([{"Id": args[2], "Name": "fake-owned-network"}]), "", 0
            if args[0] == "create":
                if "--pull=never" not in args or IMAGE not in args:
                    raise AssertionError("fake creation requires the pinned image with --pull=never")
                if IMAGE not in self.images:
                    return "", "No such image: " + IMAGE, 1
                cidfile = Path(args[args.index("--cidfile") + 1])
                if self.fixture.directory not in cidfile.parents or cidfile.exists():
                    raise AssertionError("fake creation requires its own fresh cidfile")
                cid = "original-container-" + str(len(self.containers))
                port = 41000 + len(self.containers)
                volume = "owned-volume-" + cid
                state = {"name": cidfile.stem, "running": False, "port": port, "volume": volume,
                         "env": dict(args[n + 1].split("=", 1) for n, arg in enumerate(args) if arg == "-e")}
                self.containers[cid] = self.ports[port] = state
                self.volumes.add(volume)
                cidfile.write_text(cid + "\n")
                return cid + "\n", "", 0
            if args[0] == "start":
                if not any(entry.get("id") == args[1] for entry in self.persisted().get("containers", [])):
                    self.ownership_errors.append("container ownership not durable before start")
                self.containers[args[1]]["running"] = True
                return args[1] + "\n", "", 0
            if args[0] == "inspect":
                state = self.containers[args[-1]]
                if "{{json .Mounts}}" in args:
                    value = [{"Type": "volume", "Name": state["volume"]}]
                elif "{{json .State}}" in args:
                    value = {"Running": state["running"]}
                elif "{{json .NetworkSettings.Ports}}" in args:
                    value = {"8080/tcp": [{"HostIp": "127.0.0.1", "HostPort": str(state["port"])}]}
                else:
                    value = [{"HostConfig": {"PortBindings": {}}}]
                return json.dumps(value), "", 0
            if args[0] == "logs":
                output = ""
                if self.containers[args[1]]["name"] == "origin":
                    output = "\n".join(json.dumps({"request": {"headers": {"X-Edge-Case": [case]}}}) for case in self.seen)
                return output, "", 0
            if args[0] == "rm" or args[:2] == ["network", "rm"]:
                receipt = self.persisted()
                active = [item.pid for item in self.processes if item is not process
                          and (item.returncode is None or getattr(item, "group_alive", False))]
                target = args[-1]
                owned = (receipt.get("network") == target if args[0] == "network" else
                         any(entry.get("id") == target for entry in receipt.get("containers", [])))
                self.cleanup_observations.append({"target": target, "active": active, "owned": owned})
                if self.cleanup_exception:
                    return "", "fake cleanup transport failed", 1
                if args[0] == "network":
                    self.networks.remove(target)
                else:
                    self.volumes.remove(self.containers.pop(target)["volume"])
                return target + "\n", "", 0
            if args[0] == "ps":
                return "\n".join(sorted(self.containers)), "", 0
            if args[:2] == ["volume", "ls"]:
                return "\n".join(sorted(self.volumes)), "", 0
            if args[:2] == ["network", "ls"]:
                return "\n".join(sorted(self.networks)), "", 0
            raise AssertionError("unhandled fake command: " + repr(args))

        def connection(self, host, port, timeout):
            if host != "127.0.0.1" or port not in self.ports:
                raise AssertionError("HTTP must use an explicit fake published port")
            transport, state = self, self.ports[port]

            class Connection:
                def request(self, method, path, headers):
                    self.method, self.path, self.headers = method, path, headers

                def getresponse(self):
                    if transport.completed_requests == 3 and transport.fault in ("requests-error", "requests-interrupt"):
                        fault, transport.fault = transport.fault, None
                        exception = KeyboardInterrupt if fault.endswith("interrupt") else RuntimeError
                        raise exception("after three completed fake requests")
                    path = self.path
                    # This only lets the fixture finish. It is not a routing oracle.
                    alias = path.startswith(("/admin/v1/aliases/", "/api/gateway/admin/v1/aliases/")) and path.endswith("?query=%252f")
                    if path == "/api/health" or alias and path.startswith("/api/"):
                        status, owner = 200, "console"
                    elif alias and state["env"].get("CONSOLE_AUTH_MODE"):
                        status, owner = (200, "gateway") if state["env"].get("MODELGATEWAY_UPSTREAM") else (503, None)
                    else:
                        status, owner = 404, None
                    if owner:
                        transport.seen.append(self.headers["X-Edge-Case"])
                    transport.completed_requests += 1
                    body = (self.method + " " + path).encode() if owner else b"refused"
                    return types.SimpleNamespace(status=status, getheader=lambda name: owner if name == "X-Upstream" else None,
                                                 read=lambda: body)

                def close(self):
                    pass

            return Connection()

    def setUp(self):
        self.directory = Path(tempfile.mkdtemp(prefix="alias-contract-", dir=EVIDENCE))

    def run_fixture(self, fault=None, cleanup_exception=False, configure=None):
        fixture = AliasNameRouting("test_alias_data_and_namespace_denial")
        transport = self.Transport(fixture, fault, cleanup_exception)
        if configure:
            configure(transport)
        fake_subprocess = types.SimpleNamespace(**vars(subprocess))
        fake_subprocess.Popen, fake_subprocess.run = transport.popen, transport.forbidden
        fake_os = types.SimpleNamespace(**vars(os))
        fake_os.killpg = transport.killpg
        fake_http = types.SimpleNamespace(client=types.SimpleNamespace(HTTPConnection=transport.connection,
                                                                     HTTPException=http.client.HTTPException))
        result, interrupted = unittest.TestResult(), False
        with mock.patch.dict(globals(), {"EVIDENCE": self.directory, "subprocess": fake_subprocess, "os": fake_os, "http": fake_http}):
            try:
                fixture.run(result)
            except KeyboardInterrupt:
                interrupted = True
        return fixture, transport, result, interrupted

    def assert_unrelated_untouched(self, transport):
        self.assertIn("unrelated-container", transport.containers)
        self.assertIn("unrelated-network", transport.networks)
        self.assertIn("unrelated-volume", transport.volumes)

    def assert_cleaned(self, transport):
        self.assert_unrelated_untouched(transport)
        self.assertEqual(transport.ownership_errors, [], "creation receipts must be durable before use")
        self.assertTrue(transport.cleanup_observations, "fixture never attempted owned cleanup")
        self.assertTrue(all(item["owned"] and not item["active"] for item in transport.cleanup_observations),
                        "cleanup preceded original ownership or active-child quiescence: " + repr(transport.cleanup_observations))
        self.assertEqual(set(transport.containers), {"unrelated-container"})
        self.assertEqual(transport.networks, {"unrelated-network"})
        self.assertEqual(transport.volumes, {"unrelated-volume"})
        self.assertTrue(all(process.returncode is not None for process in transport.processes),
                        "owned command still active after fixture unwound")
        self.assertFalse(any(getattr(process, "group_alive", False) for process in transport.processes),
                         "owned helper group still active after fixture unwound")

    def assert_partial_receipt(self, fixture, transport):
        receipt = transport.persisted()
        self.assertEqual(receipt.get("requests"), fixture.receipt["requests"], "lost completed requests")
        self.assertEqual(len(receipt["requests"]), 3)
        self.assertTrue({"cleanupErrors", "containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved"} <= receipt.keys(),
                        "partial receipt was not finalized after cleanup")
        return receipt

    def test_cold_image_is_prepared_before_resource_creation(self):
        fixture, transport, result, interrupted = self.run_fixture(
            configure=lambda transport: self.assertNotIn(IMAGE, transport.images))
        self.assertFalse(interrupted)
        self.assertEqual(result.errors + result.failures, [], "cold-cache setup must prepare the pinned image")
        self.assertEqual(transport.calls[0], ["pull", IMAGE], "prepare before any network or container acquisition")
        self.assertEqual(transport.calls.count(["pull", IMAGE]), 1)
        self.assertEqual(transport.images, {IMAGE})
        self.assertEqual(sum(args[:2] == ["network", "create"] for args in transport.calls), 1)
        creations = [args for args in transport.calls if args[0] == "create"]
        self.assertEqual(len(creations), 7)
        self.assertTrue(all("--pull=never" in args and IMAGE in args for args in creations))
        self.assertEqual(len(fixture.receipt["requests"]), 1464)
        self.assert_cleaned(transport)

    def test_image_preparation_failure_allocates_no_resources(self):
        fixture, transport, result, interrupted = self.run_fixture(
            "image-preparation", configure=lambda transport: self.assertNotIn(IMAGE, transport.images))
        self.assertFalse(interrupted)
        self.assertIsNone(fixture.network, "failed image preparation must not allocate a network")
        self.assertEqual(fixture.receipt["containers"], [], "failed image preparation must not attempt container creation")
        self.assertEqual(transport.calls, [["pull", IMAGE], ["ps", "-aq", "--no-trunc"],
                                         ["volume", "ls", "-q"], ["network", "ls", "--no-trunc", "-q"]])
        self.assertEqual(result.errors, [])
        self.assertEqual(len(result.failures), 1)
        self.assertIn("fake pinned image preparation failed", result.failures[0][1])
        self.assertEqual(transport.images, set())
        self.assertEqual(set(transport.containers), {"unrelated-container"})
        self.assertEqual(transport.networks, {"unrelated-network"})
        self.assertEqual(transport.volumes, {"unrelated-volume"})
        self.assertEqual(transport.cleanup_observations, [])
        self.assertEqual(transport.completed_requests, 0)
        self.assertEqual(fixture.active, {})
        self.assertTrue(all(process.returncode is not None and not process.group_alive for process in transport.processes))
        receipt = transport.persisted()
        self.assertEqual(receipt["cleanupErrors"], [])
        self.assertTrue(all(receipt[key] for key in
                            ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved")))

    def test_capture_exception_keeps_original_creation_unresolved(self):
        for fault in ("cidfile-read", "ownership-checkpoint"):
            with self.subTest(fault=fault):
                original_read, original_write = Path.read_text, Path.write_text
                injected = []

                def read(path, *args, **kwargs):
                    if fault == "cidfile-read" and path.name == "origin.cid" and path.exists() and not injected:
                        injected.append(fault)
                        raise OSError("original cidfile read failed")
                    return original_read(path, *args, **kwargs)

                def write(path, data, *args, **kwargs):
                    if fault == "ownership-checkpoint" and path.name == "receipt.json" and not injected:
                        value = json.loads(data)
                        if any("id" in entry for entry in value.get("containers", [])):
                            injected.append(fault)
                            raise OSError("original ownership checkpoint failed")
                    return original_write(path, data, *args, **kwargs)

                with mock.patch.object(Path, "read_text", read), mock.patch.object(Path, "write_text", write):
                    fixture, transport, result, interrupted = self.run_fixture()
                self.assertEqual(injected, [fault], "fault must follow original fake creation")
                self.assertFalse(interrupted)
                self.assertTrue(result.errors)
                self.assertIn("original cidfile read failed" if fault == "cidfile-read" else
                              "original ownership checkpoint failed", result.errors[0][1])
                self.assert_unrelated_untouched(transport)
                receipt = transport.persisted()
                creation = next(entry for entry in receipt["commands"] if entry["args"][0] == "create")
                with self.subTest(contract="unresolved-original-capture"):
                    self.assertTrue(creation.get("ownershipUnresolved"), "capture exception lost original ownership uncertainty")
                with self.subTest(contract="partial-error-evidence"):
                    self.assertTrue(receipt["cleanupErrors"], "capture failure vanished from cleanup evidence")
                    self.assertIn("ownershipError", creation)
                    self.assertEqual(receipt["requests"], fixture.receipt["requests"])
                with self.subTest(contract="no-unsafe-network-removal"):
                    self.assertIn("original-network-id", transport.networks)
                    self.assertFalse(any(args[:2] == ["network", "rm"] for args in transport.calls))
                for key in ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved"):
                    with self.subTest(observation=key):
                        self.assertFalse(receipt[key], "unknown original resources cannot be reported absent")
                if fault == "cidfile-read":
                    self.assertNotIn("id", receipt["containers"][0])
                    self.assertEqual(set(transport.containers), {"unrelated-container", "original-container-1"})
                    self.assertIn("owned-volume-original-container-1", transport.volumes)
                    self.assertFalse(any(args[0] in ("inspect", "rm") for args in transport.calls),
                                     "an unknown original container ID must never be guessed")

    def test_exited_leader_requires_pipe_drain_and_group_quiescence(self):
        for fault in ("helper-pipe", "helper-stop-error"):
            with self.subTest(fault=fault):
                fixture, transport, result, interrupted = self.run_fixture(fault)
                self.assertFalse(interrupted)
                self.assertTrue(result.errors)
                self.assertIn("TimeoutExpired", result.errors[0][1])
                helper = next(process for process in transport.processes if process.args[1] == "create")
                self.assertEqual(helper.returncode, 0, "the regression requires an already-exited leader")
                receipt = transport.persisted()
                creation = next(entry for entry in receipt["commands"] if entry["pid"] == helper.pid)
                (self.directory / (fault + "-observation.json")).write_text(json.dumps({
                    "leaderReturncode": helper.returncode, "helperLive": helper.helper_live,
                    "groupCalls": transport.group_calls, "cleanup": transport.cleanup_observations,
                    "active": list(fixture.active), "creation": creation}, indent=2))
                self.assert_unrelated_untouched(transport)
                if fault == "helper-pipe":
                    with self.subTest(contract="drain-and-group-stop"):
                        self.assertFalse(helper.helper_live, "leader exit is not whole-group quiescence")
                        self.assertNotIn(helper.pid, fixture.active)
                        self.assertTrue(any(call["pgid"] == helper.pid and call["signal"] in (signal.SIGTERM, signal.SIGKILL)
                                            for call in transport.group_calls))
                    with self.subTest(contract="deletion-after-quiescence"):
                        self.assert_cleaned(transport)
                        self.assertTrue(all(receipt[key] for key in
                                            ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved")))
                else:
                    with self.subTest(contract="unresolved-live-group"):
                        self.assertTrue(helper.helper_live)
                        self.assertIn(helper.pid, fixture.active, "failed drain must retain the original active group")
                        self.assertTrue(creation.get("ownershipUnresolved"))
                        self.assertIn("fake owned group could not be signalled", creation.get("reapError", ""))
                        self.assertTrue(receipt["cleanupErrors"])
                    with self.subTest(contract="no-deletion-with-live-helper"):
                        self.assertEqual(transport.cleanup_observations, [])
                        self.assertIn("original-container-1", transport.containers)
                        self.assertIn("original-network-id", transport.networks)
                    for key in ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved"):
                        with self.subTest(observation=key):
                            self.assertFalse(receipt[key], "a live owned group forbids an absence claim")

    def test_ordinary_exception_cleans_owned_resources(self):
        fixture, transport, result, interrupted = self.run_fixture("requests-error")
        self.assertFalse(interrupted)
        self.assertEqual(len(result.errors), 1)
        self.assertIn("after three completed fake requests", result.errors[0][1])
        self.assertEqual(result.failures, [])
        self.assert_cleaned(transport)
        self.assert_partial_receipt(fixture, transport)

    def test_setup_interrupt_cleans_original_network(self):
        _, transport, _, interrupted = self.run_fixture("setup")
        self.assertTrue(interrupted)
        self.assert_cleaned(transport)

    def test_delayed_network_response_is_owned_before_interrupt_cleanup(self):
        _, transport, _, interrupted = self.run_fixture("network-response")
        self.assertTrue(interrupted)
        self.assert_cleaned(transport)

    def test_delayed_cidfile_is_owned_before_interrupt_cleanup(self):
        _, transport, _, interrupted = self.run_fixture("cidfile")
        self.assertTrue(interrupted)
        self.assert_cleaned(transport)

    def test_request_interrupt_cleans_and_finalizes_partial_evidence(self):
        fixture, transport, _, interrupted = self.run_fixture("requests-interrupt")
        self.assertTrue(interrupted)
        self.assert_cleaned(transport)
        self.assert_partial_receipt(fixture, transport)

    def test_cleanup_exception_still_finalizes_partial_evidence(self):
        fixture, transport, result, interrupted = self.run_fixture("requests-error", cleanup_exception=True)
        self.assertFalse(interrupted)
        self.assertTrue(result.errors or result.failures)
        self.assert_unrelated_untouched(transport)
        receipt = self.assert_partial_receipt(fixture, transport)
        self.assertTrue(receipt["cleanupErrors"], "cleanup failure vanished from the partial receipt")

    def test_cleanup_group_failure_blocks_subsequent_resource_deletion(self):
        for stage in ("logs", "rm"):
            for fault in ("successful-output", "communicate-timeout"):
                with self.subTest(stage=stage, fault=fault):
                    injected, context = [], {}

                    def configure(transport):
                        original_popen = transport.popen

                        def popen(args, **kwargs):
                            process = original_popen(args, **kwargs)
                            communicate = process.communicate

                            def fail_during_cleanup(*args, **kwargs):
                                output = communicate(*args, **kwargs)
                                if transport.fixture.cleaning and process.args[1] == stage and not injected:
                                    injected.append(process)
                                    context.update(cleaning=transport.fixture.cleaning, args=process.args[1:],
                                                   activeBefore=[pid for pid in transport.fixture.active if pid != process.pid],
                                                   leaderReturncode=process.returncode)
                                    process.helper_live = process.group_alive = process.group_stop_error = True
                                    if fault == "communicate-timeout":
                                        raise subprocess.TimeoutExpired(process.args, kwargs.get("timeout"))
                                return output

                            process.communicate = fail_during_cleanup
                            return process

                        transport.popen = popen

                    fixture, transport, result, interrupted = self.run_fixture("requests-error", configure=configure)
                    receipt = transport.persisted()
                    (self.directory / ("cleanup-" + stage + "-" + fault + ".json")).write_text(json.dumps({
                        "injection": context, "active": list(fixture.active), "commands": receipt["commands"],
                        "cleanupErrors": receipt["cleanupErrors"], "removals": transport.cleanup_observations,
                        "remainingContainers": sorted(transport.containers), "remainingVolumes": sorted(transport.volumes),
                        "remainingNetworks": sorted(transport.networks), "groupCalls": transport.group_calls,
                        "observations": {key: receipt[key] for key in
                                         ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved")}}, indent=2))
                    self.assertFalse(interrupted)
                    self.assertEqual(len(injected), 1, "fault must occur inside a cleanup command")
                    self.assertTrue(context["cleaning"])
                    self.assertEqual(context["activeBefore"], [], "cleanup must begin without a failed group")
                    self.assertEqual(context["args"], ["logs", "original-container-1"] if stage == "logs" else
                                     ["rm", "-f", "-v", "original-container-1"])
                    self.assertEqual(context["leaderReturncode"], 0)
                    self.assertEqual(len(result.errors), 1)
                    self.assertIn("after three completed fake requests", result.errors[0][1])
                    self.assert_partial_receipt(fixture, transport)
                    self.assert_unrelated_untouched(transport)
                    helper = injected[0]
                    entry = next(item for item in receipt["commands"] if item["pid"] == helper.pid)
                    with self.subTest(contract="retained-group-and-errors"):
                        self.assertIn(helper.pid, fixture.active)
                        self.assertTrue(helper.group_alive and helper.helper_live)
                        self.assertEqual(entry["returncode"], 0)
                        self.assertIn("fake owned group could not be signalled", entry.get("reapError", ""))
                        self.assertTrue(receipt["cleanupErrors"], "cleanup settlement failure must not return success")
                        if fault == "communicate-timeout":
                            self.assertIn("TimeoutExpired", entry.get("error", ""), "preserve the original timeout")
                            self.assertIn("timed out", " ".join(receipt["cleanupErrors"]))
                        else:
                            self.assertIn("still active", " ".join(receipt["cleanupErrors"]))
                    with self.subTest(contract="no-later-destructive-admission"):
                        expected = [] if stage == "logs" else [["rm", "-f", "-v", "original-container-1"]]
                        self.assertEqual([args for args in transport.calls if args[0] == "rm" or args[:2] == ["network", "rm"]],
                                         expected, "a group failure during cleanup forbids later removals")
                        self.assertEqual([item["target"] for item in transport.cleanup_observations],
                                         [] if stage == "logs" else ["original-container-1"])
                        self.assertTrue(all(item["owned"] and not item["active"] for item in transport.cleanup_observations))
                    with self.subTest(contract="preserved-resources-and-histories"):
                        remaining = {"unrelated-container", "original-container-2"}
                        volumes = {"unrelated-volume", "owned-volume-original-container-2"}
                        if stage == "logs":
                            remaining.add("original-container-1")
                            volumes.add("owned-volume-original-container-1")
                            self.assertFalse((fixture.directory / "origin.log").exists(), "failed logs command cannot be used as success")
                        else:
                            self.assertEqual(receipt["seen"], transport.seen, "preserve logs collected before the failed removal")
                        self.assertEqual(set(transport.containers), remaining)
                        self.assertEqual(transport.volumes, volumes)
                        self.assertEqual(transport.networks, {"unrelated-network", "original-network-id"})
                        self.assertEqual(receipt["commands"], fixture.receipt["commands"])
                        self.assertEqual(receipt["expectedForwarded"], fixture.forwarded)
                        self.assertEqual(receipt["expectedRefused"], fixture.refused)
                    for key in ("containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved"):
                        with self.subTest(observation=key):
                            self.assertFalse(receipt[key], "a retained cleanup group forbids an absence claim")

    def test_cleanup_lists_are_parsed_once_and_failures_remain_unobserved(self):
        for failed_listing in (None, "containers", "volumes", "networks"):
            with self.subTest(failed_listing=failed_listing):
                counts = {"containers": 0, "volumes": 0, "networks": 0}

                class CountedOutput(str):
                    def split(self, *args, **kwargs):
                        counts[self.kind] += 1
                        return super().split(*args, **kwargs)

                def configure(transport):
                    original_execute = transport.execute

                    def execute(args, process):
                        stdout, stderr, code = original_execute(args, process)
                        kind = ("containers" if args[0] == "ps" else "volumes" if args[:2] == ["volume", "ls"]
                                else "networks" if args[:2] == ["network", "ls"] else None)
                        if kind:
                            stdout = CountedOutput(stdout)
                            stdout.kind = kind
                            if kind == failed_listing:
                                stderr, code = "fake " + kind + " listing failed", 1
                        return stdout, stderr, code

                    transport.execute = execute

                fixture, transport, result, interrupted = self.run_fixture(configure=configure)
                receipt = transport.persisted()
                (self.directory / ("listing-counts-" + (failed_listing or "success") + ".json")).write_text(json.dumps({
                    "splitCalls": counts, "failedListing": failed_listing, "requests": len(receipt["requests"]),
                    "ownedContainers": len(receipt["containers"]),
                    "ownedVolumes": sum(mount["Type"] == "volume" for entry in receipt["containers"]
                                        for mount in entry["mounts"])}, indent=2))
                self.assertFalse(interrupted)
                self.assert_cleaned(transport)
                self.assertEqual(len(receipt["requests"]), 1464)
                self.assertEqual(result.errors, [])
                if failed_listing:
                    self.assertEqual(len(result.failures), 1)
                    self.assertIn("fake " + failed_listing + " listing failed", " ".join(receipt["cleanupErrors"]))
                else:
                    self.assertEqual(result.failures, [])
                for kind, key in (("containers", "containerRemovalObserved"), ("volumes", "volumeRemovalObserved"),
                                  ("networks", "networkRemovalObserved")):
                    with self.subTest(listing=kind):
                        self.assertEqual(receipt[key], kind != failed_listing)
                        self.assertEqual(counts[kind], 0 if kind == failed_listing else 1,
                                         "parse each successful listing once, not once per owned resource")
                self.assertEqual(receipt["requests"], fixture.receipt["requests"])

    def test_all_checkpoints_serialize_histories_linearly(self):
        snapshots, writes = [], []
        original_encode, original_write = json.JSONEncoder.iterencode, Path.write_text

        def history_units(value):
            requests, results = 0, 0
            if isinstance(value, dict):
                requests += int({"case", "method", "path", "status", "owner", "body"} <= value.keys())
                results += sum(len(value.get(key, [])) for key in ("seen", "expectedForwarded", "expectedRefused"))
                for item in value.values():
                    r, s = history_units(item)
                    requests, results = requests + r, results + s
            elif isinstance(value, (list, tuple)):
                for item in value:
                    r, s = history_units(item)
                    requests, results = requests + r, results + s
            return requests, results

        def observed_encode(encoder, value, *args, **kwargs):
            requests, results = history_units(value)
            frame, caller = sys._getframe(1), "finalizer"
            while frame:
                if frame.f_code.co_name == "record":
                    caller = frame.f_back.f_code.co_name
                    break
                frame = frame.f_back
            snapshots.append({"caller": caller, "requests": requests, "results": results})
            return original_encode(encoder, value, *args, **kwargs)

        def observed_write(path, data, *args, **kwargs):
            if path.name == "receipt.json":
                writes.append(len(data.encode()))
            return original_write(path, data, *args, **kwargs)

        with mock.patch.object(json.JSONEncoder, "iterencode", observed_encode), mock.patch.object(Path, "write_text", observed_write):
            fixture, transport, result, interrupted = self.run_fixture()
        self.assertFalse(interrupted)
        self.assertEqual(result.errors + result.failures, [])
        self.assert_cleaned(transport)
        receipt = transport.persisted()
        self.assertLessEqual({"pid", "pgid", "containers", "commands", "network", "networkConfig", "originPortBindings",
                              "requests", "seen", "expectedForwarded", "expectedRefused", "cleanupErrors",
                              "containerRemovalObserved", "volumeRemovalObserved", "networkRemovalObserved"}, receipt.keys())
        self.assertEqual(receipt["requests"], fixture.receipt["requests"])
        count = len(receipt["requests"])
        self.assertEqual(count, 1464, "fake readiness succeeds on its first request")
        self.assertEqual(len(receipt["expectedForwarded"]) + len(receipt["expectedRefused"]), count)
        serialized = sum(item["requests"] for item in snapshots)
        results = sum(item["results"] for item in snapshots)
        by_caller = {name: sum(item["requests"] for item in snapshots if item["caller"] == name)
                     for name in {item["caller"] for item in snapshots}}
        (self.directory / "serialization-counts.json").write_text(json.dumps({
            "requests": count, "requestRecordsSerialized": serialized, "resultRecordsSerialized": results,
            "receiptWrites": len(writes), "receiptBytesWritten": sum(writes), "requestRecordsByCaller": by_caller}, indent=2))
        with self.subTest(work="request-records-at-all-callsites"):
            self.assertLessEqual(serialized, 2 * count, "routine checkpoints repeatedly serialize request history: " + repr(by_caller))
        with self.subTest(work="result-records-at-all-callsites"):
            self.assertLessEqual(results, 2 * sum(len(receipt[key]) for key in ("seen", "expectedForwarded", "expectedRefused")),
                                 "routine checkpoints repeatedly serialize result history")
        with self.subTest(work="receipt-bytes-written"):
            self.assertLessEqual(sum(writes), 4 * (fixture.directory / "receipt.json").stat().st_size + 2048 * len(receipt["commands"]),
                                 "receipt writes copy growing histories instead of ownership checkpoints")

    def test_detached_creation_child_is_reaped_before_cleanup(self):
        real_popen = subprocess.Popen
        ready, release = self.directory / "child-ready.json", self.directory / "child-release"
        owned, signal_errors, readiness_reads = {}, [], []
        original_read = Path.read_text
        stop = threading.Event()
        script = ("import json,os,sys,time\nfrom pathlib import Path\n"
                  "ready,release,cidfile=map(Path,sys.argv[1:])\n"
                  "ready.write_text(json.dumps({'pid':os.getpid(),'pgid':os.getpgid(0)}))\n"
                  "deadline=time.monotonic()+10\n"
                  "while not release.exists() and time.monotonic()<deadline: time.sleep(.01)\n"
                  "if release.exists():\n cidfile.write_text('original-delayed-child-id\\n')\n print('original-delayed-child-id',flush=True)\n")

        def configure(transport):
            def factory(args, kwargs):
                if args[1] != "create" or owned:
                    return None
                cidfile = args[args.index("--cidfile") + 1]
                process = real_popen([sys.executable, "-I", "-c", script, str(ready), str(release), cidfile],
                                     stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, start_new_session=True)
                owned["process"] = process
                # Preserve the actual original PID/group before any signal.
                identity = {"pid": process.pid, "pgid": os.getpgid(process.pid)}
                if identity["pid"] != identity["pgid"] or identity["pgid"] == os.getpgid(0):
                    raise AssertionError("fake child must own a separate process group")
                owned.update(identity)
                (self.directory / "original-child-identity.json").write_text(json.dumps(identity))
                transport.real_groups[process.pid] = identity.copy()
                communicate = process.communicate

                def interrupted_communicate(*args, **kwargs):
                    try:
                        output = communicate(*args, **kwargs)
                    except KeyboardInterrupt:
                        release.write_text("finish original creation")
                        raise
                    if Path(cidfile).exists():
                        cid = Path(cidfile).read_text().strip()
                        transport.containers[cid] = {"name": "origin", "running": False, "port": 42000,
                                                     "volume": "owned-child-volume", "env": {}}
                        transport.volumes.add("owned-child-volume")
                    return output

                process.communicate = interrupted_communicate

                def interrupt_inside_communicate():
                    try:
                        deadline = time.monotonic() + 5
                        while not stop.wait(.01) and time.monotonic() < deadline:
                            if not ready.exists():
                                continue
                            try:
                                metadata = json.loads(ready.read_text())
                            except json.JSONDecodeError:
                                # The path exists before write_text finishes publication.
                                continue
                            if metadata != identity:
                                raise AssertionError("child identity changed before signal")
                            frame = sys._current_frames()[threading.main_thread().ident]
                            while frame:
                                if frame.f_code.co_filename == subprocess.__file__ and frame.f_code.co_name == "_communicate":
                                    os.kill(os.getpid(), signal.SIGINT)
                                    return
                                frame = frame.f_back
                        if not stop.is_set():
                            raise AssertionError("did not observe real communicate before SIGINT")
                    except Exception as error:
                        signal_errors.append(str(error))

                thread = threading.Thread(target=interrupt_inside_communicate)
                owned["thread"] = thread
                thread.start()
                return process
            transport.child_factory = factory

        def incomplete_readiness_once(path, *args, **kwargs):
            if path == ready and not readiness_reads:
                readiness_reads.append("{")
                return "{"
            value = original_read(path, *args, **kwargs)
            if path == ready:
                readiness_reads.append(value)
            return value

        try:
            with mock.patch.object(Path, "read_text", incomplete_readiness_once):
                _, transport, _, interrupted = self.run_fixture(configure=configure)
            self.assertTrue(interrupted, "real child-creation SIGINT was not propagated")
            self.assertEqual(signal_errors, [])
            self.assertEqual(readiness_reads[0], "{")
            self.assertGreaterEqual(len(readiness_reads), 2, "the observer must retry incomplete readiness")
            self.assertEqual(json.loads(readiness_reads[-1]), {"pid": owned["pid"], "pgid": owned["pgid"]})
            self.assertIsNotNone(owned["process"].returncode, "fixture unwound without reaping its owned detached creation child")
            self.assert_cleaned(transport)
        finally:
            stop.set()
            if "thread" in owned:
                owned["thread"].join(6)
            if "process" in owned:
                process = owned["process"]
                if process.returncode is None:
                    try:
                        if os.getpgid(owned["pid"]) == owned["pgid"]:
                            os.killpg(owned["pgid"], signal.SIGTERM)
                    except ProcessLookupError:
                        # The owned child can exit before this final group check.
                        pass
                process.communicate(timeout=5)
                listing = subprocess.run(["ps", "-axo", "pid=,pgid=,comm="], capture_output=True, text=True, check=True)
                remaining = [line for line in listing.stdout.splitlines()
                             if any(int(value) in (owned["pid"], owned["pgid"]) for value in line.split(None, 2)[:2])]
                (self.directory / "child-final-observation.json").write_text(json.dumps({
                    "pid": owned["pid"], "pgid": owned["pgid"], "returncode": process.returncode,
                    "processListingSucceeded": listing.returncode == 0, "remaining": remaining,
                    "observerAlive": owned["thread"].is_alive() if "thread" in owned else False,
                    "readinessReads": readiness_reads, "signalErrors": signal_errors}, indent=2))
                self.assertEqual(remaining, [], "owned child PID/group survived teardown")
            if "thread" in owned:
                self.assertFalse(owned["thread"].is_alive(), "owned interrupt observer survived teardown")


if __name__ == "__main__":
    unittest.main(verbosity=2)
