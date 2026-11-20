#!/usr/bin/env python3
"""
The corpus of dataplane/e2e/ingress-nginx-parity.sh: the Ingresses of the
corpus files with what they need to be served the same by both controllers,
and the probes of each.

    corpus.py <backends.json> <out-dir> <corpus files...>

backends.json are the addresses of the backend pods: {"echo": [...],
"ws": [...], "grpc": [...], "auth": [...], "errors": [...]}. The Services an
Ingress names are made without a selector, with an EndpointSlice of the pod
that suits the Ingress - the echo server (http 8080, https 8443), the
WebSocket echo, grpcbin, the auth service - so the corpus needs no
application of its own. Its TLS Secrets are one certificate for every host
(tls.crt, tls.key in out-dir, made by the orchestrator), its basic auth
Secrets have the user "user" with the password "secret".

It writes out-dir/manifests.yaml and out-dir/probes.json.
"""

import json
import os
import re
import sys

import_yaml = None


def load_yaml(path):
    global import_yaml
    if import_yaml is None:
        import yaml  # noqa: PLC0415 - the orchestrator's machine has PyYAML; the probe pod does not need it
        import_yaml = yaml
    return [d for d in import_yaml.safe_load_all(open(path)) if d]


def ingresses_of(path):
    if path.endswith(".json"):
        doc = json.load(open(path))
        items = doc["items"] if isinstance(doc, dict) and "items" in doc else doc
    else:
        items = load_yaml(path)
    return [i for i in items if i.get("kind", "Ingress") == "Ingress"]


P = "nginx.ingress.kubernetes.io/"
HTPASSWD = "user:$apr1$parity00$MhVQfdm4Y7Bcjd6s9IW0e/"  # openssl passwd -apr1 -salt parity00 secret


def backend_kind(ing, port):
    a = ing["metadata"].get("annotations") or {}
    proto = a.get(P + "backend-protocol", "HTTP").upper()
    if proto in ("GRPC", "GRPCS"):
        return "grpc", 9001 if proto == "GRPCS" else 9000
    if proto == "HTTPS":
        return "echo", 8443
    if a.get("parity/backend") == "ws":
        return "ws", 8080
    if a.get("parity/backend") == "auth":
        return "auth", 8080
    if a.get("parity/backend") == "errors":
        return "errors", 8080
    if a.get("parity/backend") == "none":
        return "none", 8080
    return "echo", 8080


def main():
    backends = json.load(open(sys.argv[1]))
    out = sys.argv[2]
    docs, probes = [], []
    namespaces, services, secrets = set(), {}, set()
    ingresses = []
    for path in sys.argv[3:]:
        for ing in ingresses_of(path):
            ingresses.append(ing)

    def service(ns, name, port_ref, kind, target):
        key = (ns, name)
        svc = services.setdefault(key, {"kind": kind, "ports": {}})
        if port_ref.get("name"):
            pname = port_ref["name"]
            if pname in svc["ports"]:
                return
            # a port the Ingress names: its number is free to choose, one the Service does not have yet
            taken = {n for n, _ in svc["ports"].values()}
            number = next(n for n in [80] + list(range(8000, 9000)) if n not in taken)
        else:
            number = port_ref.get("number") or 80
            pname = f"p{number}"
            if any(n == number for n, _ in svc["ports"].values()):
                return
        svc["ports"][pname] = (number, target)

    for ing in ingresses:
        meta = ing["metadata"]
        ns = meta.get("namespace", "default")
        meta.pop("resourceVersion", None)
        meta.pop("uid", None)
        meta.pop("creationTimestamp", None)
        meta.pop("managedFields", None)
        ing.pop("status", None)
        namespaces.add(ns)
        spec = ing["spec"]
        backends_of = []
        if spec.get("defaultBackend", {}).get("service"):
            backends_of.append(spec["defaultBackend"]["service"])
        for rule in spec.get("rules", []):
            for p in (rule.get("http") or {}).get("paths", []):
                if p.get("backend", {}).get("service"):
                    backends_of.append(p["backend"]["service"])
        a = meta.get("annotations") or {}
        for b in backends_of:
            kind, target = backend_kind(ing, b.get("port", {}))
            service(ns, b["name"], b.get("port", {}), kind, target)
        if P + "default-backend" in a:
            service(ns, a[P + "default-backend"], {"number": 80}, "errors", 8080)
        for t in spec.get("tls", []):
            if t.get("secretName"):
                secrets.add((ns, t["secretName"], "tls"))
        if a.get(P + "auth-type") == "basic" and P + "auth-secret" in a:
            secrets.add((ns, a[P + "auth-secret"], "basic"))

        # the probes of each rule's paths
        tls_hosts = {h for t in spec.get("tls", []) for h in t.get("hosts", [])}
        ident = f"{ns}/{meta['name']}"
        for rule in spec.get("rules", []) or [{"host": "", "http": {"paths": [{"path": "/"}]}}]:
            host = rule.get("host") or "catchall.parity.test"
            if host.startswith("*."):
                host = "wild" + host[1:]
            for p in (rule.get("http") or {}).get("paths", []) or [{"path": "/"}]:
                path = p.get("path") or "/"
                regex = bool(re.search(r"[()\[\]|$^*+?\\]", path))
                base = path if not regex else re.sub(r"\(.*", "", path) or "/"
                variants = {"exact": base}
                if not regex:
                    variants.update({
                        "slash": base.rstrip("/") + "/", "sub": base.rstrip("/") + "/sub/x?q=1",
                        "boundary": base.rstrip("/") + "x", "upper": base.upper(),
                    })
                for name, v in variants.items():
                    for scheme in ("http", "https"):
                        probes.append({"id": f"{ident} {host}{path} {name} {scheme}", "host": host, "path": v or "/", "scheme": scheme,
                                       "load": name == "exact" and scheme == ("https" if host in tls_hosts else "http")})
                q = f"{ident} {host}{path}"
                scheme = "https" if host in tls_hosts else "http"
                probes.append({"id": q + " websocket", "host": host, "path": base, "scheme": scheme, "headers": {
                    "Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ==",
                    "Sec-WebSocket-Version": "13"}})
                probes.append({"id": q + " preflight", "host": host, "path": base, "scheme": scheme, "method": "OPTIONS",
                               "headers": {"Origin": "https://app.parity.test", "Access-Control-Request-Method": "PUT"}})
                probes.append({"id": q + " cors", "host": host, "path": base, "scheme": scheme,
                               "headers": {"Origin": "https://app.parity.test"}})
                probes.append({"id": q + " forwarded", "host": host, "path": base, "scheme": scheme, "headers": {
                    "X-Forwarded-For": "198.51.100.7", "X-Forwarded-Proto": "https", "X-Forwarded-Host": "evil.test"}})
                probes.append({"id": q + " body2m", "host": host, "path": base, "scheme": scheme, "method": "POST", "body": 2 << 20})
                probes.append({"id": q + " basic", "host": host, "path": base, "scheme": scheme,
                               "headers": {"Authorization": "Basic dXNlcjpzZWNyZXQ="}})
                probes.append({"id": q + " canary", "host": host, "path": base, "scheme": scheme,
                               "headers": {"X-Canary": "always"}})
    probes.append({"id": "unknown host http", "host": "unknown.parity.test", "path": "/", "scheme": "http"})
    probes.append({"id": "unknown host https", "host": "unknown.parity.test", "path": "/", "scheme": "https"})

    for ns in sorted(namespaces):
        docs.append({"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": ns}})
    for (ns, name), svc in sorted(services.items()):
        ports = [{"name": n, "port": num, "targetPort": target, "protocol": "TCP"} for n, (num, target) in sorted(svc["ports"].items())]
        docs.append({"apiVersion": "v1", "kind": "Service", "metadata": {"name": name, "namespace": ns},
                     "spec": {"ports": ports}})
        docs.append({"apiVersion": "discovery.k8s.io/v1", "kind": "EndpointSlice",
                     "metadata": {"name": name + "-parity", "namespace": ns, "labels": {"kubernetes.io/service-name": name}},
                     "addressType": "IPv4",
                     "ports": [{"name": n, "port": target, "protocol": "TCP"} for n, (num, target) in sorted(svc["ports"].items())],
                     "endpoints": [{"addresses": [ip], "conditions": {"ready": True}} for ip in backends.get(svc["kind"], [])]})
    # the hosts for the one certificate of every TLS Secret: the orchestrator makes it, then runs this again
    hosts = sorted({(r.get("host") or "catchall.parity.test") for i in ingresses for r in i["spec"].get("rules", [])} |
                   {h for i in ingresses for t in i["spec"].get("tls", []) for h in t.get("hosts", [])})
    open(os.path.join(out, "hosts.txt"), "w").write("\n".join(hosts) + "\n")
    if not os.path.exists(os.path.join(out, "tls.crt")):
        print(f"{len(hosts)} hosts in {out}/hosts.txt; make tls.crt and tls.key for them")
        sys.exit(3)
    crt = open(os.path.join(out, "tls.crt")).read()
    key = open(os.path.join(out, "tls.key")).read()
    import base64  # noqa: PLC0415
    b64 = lambda s: base64.b64encode(s.encode()).decode()  # noqa: E731
    for ns, name, kind in sorted(secrets):
        if kind == "tls":
            docs.append({"apiVersion": "v1", "kind": "Secret", "type": "kubernetes.io/tls", "metadata": {"name": name, "namespace": ns},
                         "data": {"tls.crt": b64(crt), "tls.key": b64(key)}})
        else:
            docs.append({"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": {"name": name, "namespace": ns},
                         "data": {"auth": b64(HTPASSWD + "\n")}})
    docs.extend(ingresses)
    with open(os.path.join(out, "manifests.yaml"), "w") as f:
        for d in docs:
            f.write("---\n" + json.dumps(d) + "\n")
    json.dump(probes, open(os.path.join(out, "probes.json"), "w"), indent=1)
    print(f"{len(ingresses)} Ingresses, {len(services)} Services, {len(secrets)} Secrets, {len(probes)} probes")


if __name__ == "__main__":
    main()
