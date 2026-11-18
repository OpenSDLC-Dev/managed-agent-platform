#!/usr/bin/env python3
"""Render staging's existing HTTPS edge onto the platform-owned application edge."""
import argparse
import json
from pathlib import Path
import re
from urllib.parse import urlsplit


def validate_existing(origin, resources):
    """Refuse a cutover that would replace the owner's existing TLS identity."""
    items = {(item["kind"], item["metadata"]["name"]): item
             for item in resources.get("items", [])}
    ingress = items.get(("Ingress", "console"), {})
    certificate = items.get(("ManagedCertificate", "console"), {})
    hosts = [rule.get("host") for rule in ingress.get("spec", {}).get("rules", [])]
    domains = certificate.get("spec", {}).get("domains", [])
    if hosts != [origin.removeprefix("https://")] or domains != hosts:
        raise ValueError("origin must match the existing console Ingress host and ManagedCertificate domain")
    annotations = ingress.get("metadata", {}).get("annotations", {})
    for key, expected in {
        "kubernetes.io/ingress.global-static-ip-name": "console-ip",
        "networking.gke.io/managed-certificates": "console",
        "networking.gke.io/v1beta1.FrontendConfig": "console",
    }.items():
        if annotations.get(key) != expected:
            raise ValueError(f"existing console Ingress must retain {key}={expected}")


def manifest(origin, service):
    parsed = urlsplit(origin)
    host = parsed.hostname or ""
    label = r"[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?"
    if (origin != "https://" + host or len(host) > 253
            or not re.fullmatch(rf"{label}(?:\.{label})*\.[a-z]{{2,63}}", host)):
        raise ValueError("origin must be a canonical HTTPS DNS origin without a port or path")
    if not re.fullmatch(r"[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?", service):
        raise ValueError("service must be a Kubernetes Service name")

    def obj(api, kind, name, spec, annotations=None):
        metadata = {"name": name, "labels": {"app.kubernetes.io/part-of": "managed-agent-platform"}}
        if annotations:
            metadata["annotations"] = annotations
        return {"apiVersion": api, "kind": kind, "metadata": metadata, "spec": spec}

    # Keep the existing Ingress, address and certificate names. GKE can collect
    # the old cloud backend and its IAM grant after the service switch.
    # Restoring the IAP-backed service requires that grant (see README).
    return {"apiVersion": "v1", "kind": "List", "items": [
        obj("cloud.google.com/v1", "BackendConfig", "map-native-edge", {
            "iap": {"enabled": False}, "timeoutSec": 3600,
            "healthCheck": {"type": "HTTP", "requestPath": "/api/health", "port": 8080},
        }),
        obj("networking.gke.io/v1", "ManagedCertificate", "console", {"domains": [host]}),
        obj("networking.gke.io/v1beta1", "FrontendConfig", "console", {
            "redirectToHttps": {"enabled": True},
        }),
        obj("networking.k8s.io/v1", "Ingress", "console", {
            "rules": [{"host": host, "http": {"paths": [{
                "path": "/*", "pathType": "ImplementationSpecific",
                "backend": {"service": {"name": service, "port": {"number": 8080}}},
            }]}}],
        }, {
            "kubernetes.io/ingress.class": "gce",
            "kubernetes.io/ingress.global-static-ip-name": "console-ip",
            "networking.gke.io/managed-certificates": "console",
            "networking.gke.io/v1beta1.FrontendConfig": "console",
        }),
    ]}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--origin", required=True)
    parser.add_argument("--service", required=True)
    parser.add_argument("--existing-edge", type=Path,
                        help="kubectl JSON List of the existing Ingress and ManagedCertificate")
    args = parser.parse_args()
    try:
        rendered = manifest(args.origin, args.service)
        if args.existing_edge:
            validate_existing(args.origin, json.loads(args.existing_edge.read_text()))
        print(json.dumps(rendered))
    except ValueError as error:
        parser.error(str(error))
