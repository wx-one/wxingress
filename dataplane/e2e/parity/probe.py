#!/usr/bin/env python3
"""
The probes of dataplane/e2e/ingress-nginx-parity.sh, run in a pod of the
cluster against the hostPort of one node: every request of probes.json,
recorded as what a client and the backend saw.

    probe.py probe <node-ip> <probes.json> <out.json>
    probe.py load <pool-file> <probes.json> <stats-file> <stop-file>

A probe is {"id", "host", "path", "scheme", "method", "headers", "body"}.
What is recorded of an answer: its status; the headers that tell behaviour
(Location, HSTS, CORS, auth, cookies without their values, the content type,
and those of the custom-headers ConfigMaps); and, where the backend is the
echo server (mendhak/http-https-echo answers JSON), what the backend saw -
the method, path, query, and the headers a controller sets. Another body is
recorded as a digest, its nginx version taken out. Only the standard library:
the probe pod is python:3.12-alpine.

`load` sends requests round the nodes of the pool file - which the
orchestrator rewrites to take a node out before it is switched and to put it
back after - and counts what failed: no answer, or a 5xx where the probe's
answer was not one. It stops when the stop file exists.
"""

import hashlib
import http.client
import json
import os
import re
import socket
import ssl
import sys
import threading
import time

TIMEOUT = 5

KEEP_HEADERS = [
    "location", "strict-transport-security", "www-authenticate", "content-type",
    "access-control-allow-origin", "access-control-allow-methods", "access-control-allow-headers",
    "access-control-allow-credentials", "access-control-expose-headers", "access-control-max-age",
    "x-inx-custom", "x-frame-options", "upgrade", "grpc-status",
]

BACKEND_HEADERS = [
    "host", "x-forwarded-for", "x-forwarded-proto", "x-forwarded-host", "x-forwarded-port",
    "x-forwarded-scheme", "x-scheme", "x-real-ip", "x-original-uri", "x-original-url",
    "x-forwarded-prefix", "connection", "upgrade", "x-auth-request-user", "x-code", "x-format",
    "x-original-forwarded-for", "x-inx-tenant",
]


def context():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    ctx.set_alpn_protocols(["http/1.1"])
    return ctx


CTX = context()


def corpus_certificate():
    """The digest of the corpus's one certificate (/p/tls.crt): a server that shows it shows the host's own."""
    try:
        return hashlib.sha1(ssl.PEM_cert_to_DER_cert(open("/p/tls.crt").read())).hexdigest()[:12]
    except OSError:
        return None


CORPUS = corpus_certificate()


def request(node, probe):
    """One request to the node as the probe says: the response, its body, and the certificate's subject over TLS."""
    port = 443 if probe["scheme"] == "https" else 80
    sock = socket.create_connection((node, port), timeout=TIMEOUT)
    subject = None
    if probe["scheme"] == "https":
        sock = CTX.wrap_socket(sock, server_hostname=probe.get("sni", probe["host"]))
        der = sock.getpeercert(binary_form=True)
        digest = hashlib.sha1(der).hexdigest()[:12] if der else None
        # the host's own certificate, or the controller's default one, whichever that is
        subject = "the host's" if digest == CORPUS else "a default"
    conn = http.client.HTTPConnection(probe["host"], port, timeout=TIMEOUT)
    conn.sock = sock
    headers = {"Host": probe["host"], "User-Agent": "parity-probe"}
    headers.update(probe.get("headers", {}))
    body = probe.get("body")
    if isinstance(body, int):
        body = b"x" * body
    elif isinstance(body, str):
        body = body.encode()
    conn.request(probe.get("method", "GET"), probe["path"], body=body, headers=headers)
    resp = conn.getresponse()
    data = b""
    if resp.status != 101:
        data = resp.read(8 << 20)
    conn.close()
    return resp, data, subject


def normalized_body(data):
    text = re.sub(rb"nginx/[0-9.]+", b"nginx", data)
    return hashlib.sha1(text).hexdigest()[:12]


def record(node, probe):
    out = {"id": probe["id"]}
    try:
        resp, data, subject = request(node, probe)
    except Exception as e:  # noqa: BLE001 - every failure is a result
        out["error"] = type(e).__name__
        return out
    out["status"] = resp.status
    if subject:
        out["certificate"] = subject
    headers = {}
    for name in KEEP_HEADERS:
        values = resp.headers.get_all(name)
        if values:
            headers[name] = ", ".join(values)
    cookies = resp.headers.get_all("set-cookie") or []
    if cookies:
        # the name and the attributes, not the value: a session differs per request
        headers["set-cookie"] = sorted(
            ";".join([c.split(";")[0].split("=")[0]] + sorted(p.strip().split("=")[0].lower() for p in c.split(";")[1:]))
            for c in cookies)
    if "content-type" in headers:
        headers["content-type"] = headers["content-type"].split(";")[0].strip().lower()
    out["headers"] = headers
    echo = None
    try:
        echo = json.loads(data)
    except (ValueError, UnicodeDecodeError):
        pass
    if isinstance(echo, dict) and "headers" in echo and "path" in echo:
        seen = {k: v for k, v in echo["headers"].items() if k.lower() in BACKEND_HEADERS}
        if "x-request-id" in echo["headers"]:
            seen["x-request-id"] = "present"
        out["backend"] = {
            "method": echo.get("method"), "path": echo.get("path"), "query": echo.get("query"),
            "headers": seen, "body": len(echo.get("body") or ""),
            "tls": (echo.get("connection") or {}).get("servername") is not None,
        }
    elif data:
        out["body"] = normalized_body(data)
    return out


def probe_all(node, probes_file, out_file):
    probes = json.load(open(probes_file))
    results = {}
    lock = threading.Lock()
    todo = list(probes)

    def worker():
        while True:
            with lock:
                if not todo:
                    return
                p = todo.pop()
            r = record(node, p)
            with lock:
                results[p["id"]] = r

    threads = [threading.Thread(target=worker) for _ in range(16)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    json.dump(results, open(out_file, "w"), indent=1, sort_keys=True)
    print(f"{len(results)} probes of {node} in {out_file}")


def load(pool_file, probes_file, stats_file, stop_file):
    """Requests round the pool's nodes until the stop file is there: how many, and how many failed."""
    probes = [p for p in json.load(open(probes_file)) if p.get("load")]
    stats = {"sent": 0, "failed": 0, "failures": []}
    lock = threading.Lock()
    counter = [0]

    def worker():
        while not os.path.exists(stop_file):
            try:
                pool = open(pool_file).read().split()
            except OSError:
                pool = []
            if not pool:
                time.sleep(0.1)
                continue
            with lock:
                counter[0] += 1
                n = counter[0]
            node = pool[n % len(pool)]
            p = probes[n % len(probes)]
            failed = None
            try:
                resp, _, _ = request(node, p)
                if resp.status >= 500 and not p.get("expect5xx"):
                    failed = f"{resp.status}"
            except Exception as e:  # noqa: BLE001
                failed = type(e).__name__
            with lock:
                stats["sent"] += 1
                if failed:
                    stats["failed"] += 1
                    if len(stats["failures"]) < 50:
                        stats["failures"].append({"t": time.strftime("%H:%M:%S"), "node": node, "id": p["id"], "what": failed})
                if stats["sent"] % 200 == 0:
                    json.dump(stats, open(stats_file + ".tmp", "w"))
                    os.replace(stats_file + ".tmp", stats_file)
            time.sleep(0.01)

    threads = [threading.Thread(target=worker) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    json.dump(stats, open(stats_file, "w"))
    print(json.dumps({"sent": stats["sent"], "failed": stats["failed"]}))


if __name__ == "__main__":
    if sys.argv[1] == "probe":
        probe_all(sys.argv[2], sys.argv[3], sys.argv[4])
    elif sys.argv[1] == "load":
        load(sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5])
    else:
        sys.exit(__doc__)
