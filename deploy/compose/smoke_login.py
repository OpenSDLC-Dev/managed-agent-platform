#!/usr/bin/env python3
"""Smoke a fresh development install through setup, login and private issuance.

Run beside docker-compose.yml. The sole output credential is a mode-0600 file
for later public-API checks. Never upload that file or the temporary cookie jar.
"""
import argparse
import http.cookiejar
import json
import os
from pathlib import Path
import re
import secrets
import subprocess
import tempfile
import time
import urllib.error
import urllib.request


def setup_path(logs):
    paths = []
    for line in logs.splitlines():
        try:
            path = json.loads(line).get("setup_code_file", "")
        except (ValueError, AttributeError):
            # Server telemetry emits slog text in real processes.
            match = re.search(r"(?:^|\s)setup_code_file=(/tmp/map-setup-[0-9]+/setup-code)(?:\s|$)", line)
            path = match.group(1) if match else ""
        if isinstance(path, str) and re.fullmatch(r"/tmp/map-setup-[0-9]+/setup-code", path):
            paths.append(path)
    if not paths:
        raise RuntimeError("fresh control plane did not report a restricted setup-code file")
    return paths[-1]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--key-file", type=Path, required=True)
    args = parser.parse_args()
    base, gateway, origin = "http://localhost:8080", "http://localhost:8090", "http://localhost:3000"
    jar = http.cookiejar.CookieJar()
    browser = urllib.request.build_opener(urllib.request.HTTPCookieProcessor(jar))

    def call(url, body=None, csrf=None, key=None, expected=200, opener=browser):
        headers = {"Origin": origin}
        if body is not None:
            headers["Content-Type"] = "application/json"
        if csrf:
            headers["X-CSRF-Token"] = csrf
        if key:
            headers["x-api-key"] = key
        request = urllib.request.Request(url, data=None if body is None else json.dumps(body).encode(), headers=headers)
        try:
            response = opener.open(request, timeout=10)
        except urllib.error.HTTPError as error:
            response = error
        with response:
            if response.status != expected:
                raise RuntimeError(f"{request.method} {url}: status {response.status}, expected {expected}")
            return json.load(response)

    for attempt in range(30):
        try:
            config = call(base + "/api/auth/config")
            break
        except (OSError, RuntimeError):
            if attempt == 29:
                raise RuntimeError("control plane did not become ready") from None
            time.sleep(2)
    if config != {"mode": "password", "native_login": True}:
        raise RuntimeError("Compose must start with native password login")

    logs = subprocess.run(["docker", "compose", "logs", "--no-color", "--no-log-prefix", "controlplane"], capture_output=True, text=True, check=True).stdout
    path = setup_path(logs)

    with tempfile.TemporaryDirectory(prefix="map-smoke-login-") as directory:
        os.chmod(directory, 0o700)
        local = Path(directory) / "setup-code"
        subprocess.run(["docker", "compose", "cp", "controlplane:" + path, str(local)], capture_output=True, check=True)
        code = local.read_text().strip()
    credentials = {"login_name": "ci-admin", "password": secrets.token_urlsafe(24)}
    setup = call(base + "/api/console/organizations/default/setup_code/redeem", {"code": code, **credentials})
    call(base + "/api/auth/logout", {}, setup["csrf_token"])
    session = call(base + "/api/auth/login", credentials)
    if not session["signed_in"] or session["role"] != "admin":
        raise RuntimeError("setup/login did not establish an admin session")
    issued = call(base + "/api/console/organizations/default/workspaces/default/api_keys", {"name": "ci-smoke"}, session["csrf_token"])
    if issued["principal"] != {"type": "user_actor", "user_id": session["id"]}:
        raise RuntimeError("issued key must belong to the signed-in admin")
    key = issued["raw_key"]
    public = urllib.request.build_opener()
    call(base + "/v1/agents", key=key, opener=public)
    call(base + "/v1/agents", expected=401, opener=public)
    for attempt in range(30):
        try:
            call(gateway + "/admin/v1/profiles")
            break
        except (OSError, RuntimeError):
            if attempt == 29:
                raise RuntimeError("gateway did not accept the Console cookie") from None
            time.sleep(2)
    call(gateway + "/admin/v1/profiles", expected=401, opener=public)
    call(gateway + "/admin/v1/profiles", key=key, expected=401, opener=public)
    call(gateway + "/v1/models", key=key, opener=public)
    call(base + "/api/auth/logout", {}, session["csrf_token"])
    fd = os.open(args.key_file, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        output.write(key)
    print("ok: setup, login, owned-key issuance, public API and cookie-authenticated gateway administration")


if __name__ == "__main__":
    main()
