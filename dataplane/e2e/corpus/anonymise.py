#!/usr/bin/env python3
"""Make a test corpus out of a cluster's Ingresses without its names.

    kubectl get ingress -A -o json > /somewhere/outside/the/repo/ing.json
    python3 -I dataplane/e2e/corpus/anonymise.py /somewhere/.../ing.json \
        > dataplane/e2e/corpus/<name>/ingresses.json

Replaced, consistently (the same original gives the same placeholder
everywhere, so hosts of a rule and of its TLS entry, or two Ingresses of one
namespace, still match):

  namespaces          ns-N
  Ingress names       ing-N
  Service names       svc-N
  Secret names        secret-N
  issuer names        issuer-N        (cert-manager.io/[cluster-]issuer)
  Helm releases       release-N       (meta.helm.sh/release-name)
  hosts               hN.zoneM.test   (zoneM per registered domain, a
                                       leading "*." stays)
  in annotation values and paths: every host, every <svc>.<ns>[.svc...]
  and every other original name above.

Dropped: uid, resourceVersion, generation, creationTimestamp, managedFields,
labels, status, kubectl.kubernetes.io/last-applied-configuration and the
annotations of Rancher (field.cattle.io/*, objectset.rio.cattle.io/*).

Kept as they are: annotation keys and the shape of their values, paths,
pathTypes, ports (numbers and names), TLS entries, ingressClassName or its
absence, and the order of everything.

The last step looks for every original name (and every label of every host
longer than three characters that is not a common word of hosts) in the
output and fails, writing nothing, if one is left.
"""

import json
import re
import sys

DROP_META = ("uid", "resourceVersion", "generation", "creationTimestamp",
             "managedFields", "labels", "selfLink", "ownerReferences",
             "finalizers")
DROP_ANNOTATION = re.compile(
    r"^(kubectl\.kubernetes\.io/last-applied-configuration"
    r"|field\.cattle\.io/.*|objectset\.rio\.cattle\.io/.*)$")
ISSUER = ("cert-manager.io/cluster-issuer", "cert-manager.io/issuer")
RELEASE = "meta.helm.sh/release-name"
RELEASE_NS = "meta.helm.sh/release-namespace"
# host labels too common to give anything away; they are replaced all the
# same, only the final check does not insist on them
COMMON = {"www", "api", "app", "apps", "auth", "mail", "chat", "docs",
          "test", "demo", "prod", "stage", "staging", "dev", "admin",
          "static", "cdn", "login", "matrix", "element", "office", "cloud",
          "local", "cluster", "svc", "http", "https", "nginx", "proxy",
          "public", "status", "metrics", "grafana", "gitlab", "registry"}


class Names:
    def __init__(self, prefix):
        self.prefix = prefix
        self.map = {}

    def __call__(self, name):
        if name not in self.map:
            self.map[name] = "%s-%d" % (self.prefix, len(self.map) + 1)
        return self.map[name]


ns = Names("ns")
ing = Names("ing")
svc = Names("svc")
secret = Names("secret")
issuer = Names("issuer")
release = Names("release")
zones = {}
hosts = {}


def zone_of(host):
    labels = host.split(".")
    # two labels as the registered domain; a second-level suffix as co.uk
    # makes it three
    n = 3 if len(labels) > 2 and len(labels[-2]) <= 3 and len(labels[-1]) == 2 else 2
    return ".".join(labels[-n:])


def host(h):
    if not h:
        return h
    wild = h.startswith("*.")
    bare = h[2:] if wild else h
    if bare not in hosts:
        z = zone_of(bare)
        if z not in zones:
            zones[z] = "zone%d.test" % (len(zones) + 1)
        if bare == z:
            hosts[bare] = zones[z]
        else:
            hosts[bare] = "h%d.%s" % (len(hosts) + 1, zones[z])
    return ("*." if wild else "") + hosts[bare]


def text(value):
    """Hosts and names inside an annotation value or a path."""
    if not value:
        return value
    out = value
    # in-cluster names: <svc>.<ns>[.svc[.cluster.local]]
    def incluster(m):
        return "%s.%s%s" % (svc(m.group(1)), ns(m.group(2)), m.group(3) or "")
    known_ns = "|".join(re.escape(n) for n in sorted(ns.map, key=len, reverse=True))
    if known_ns:
        out = re.sub(r"\b([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)\.(%s)(\.svc(?:\.cluster\.local)?)?\b" % known_ns,
                     incluster, out)
    for original in sorted(hosts, key=len, reverse=True):
        out = re.sub(r"(?<![A-Za-z0-9.-])%s(?![A-Za-z0-9-])" % re.escape(original), hosts[original], out)
    # any other host in a URL
    def url(m):
        h = m.group(2)
        if h.endswith(".test") or re.search(r"\.svc(\.cluster\.local)?$", h):
            return m.group(0)  # replaced above
        return m.group(1) + host(h)
    out = re.sub(r"(https?://)([A-Za-z0-9.-]+\.[A-Za-z]{2,})", url, out)
    # names on their own, as a whole word
    for names in (ns, ing, svc, secret, issuer, release):
        for original in sorted(names.map, key=len, reverse=True):
            if len(original) > 3:
                out = re.sub(r"(?<![A-Za-z0-9])%s(?![A-Za-z0-9])" % re.escape(original),
                             names.map[original], out)
    return out


def backend(b):
    if b and "service" in b:
        b["service"]["name"] = svc(b["service"]["name"])
    return b


def first_pass(items):
    """Every name before any text, so that text() knows them all."""
    for i in items:
        m = i["metadata"]
        ns(m["namespace"])
        ing(m["name"])
        a = m.get("annotations", {})
        for k in ISSUER:
            if k in a:
                issuer(a[k])
        if RELEASE in a:
            release(a[RELEASE])
        if RELEASE_NS in a:
            ns(a[RELEASE_NS])
        s = i["spec"]
        if s.get("defaultBackend", {}).get("service"):
            svc(s["defaultBackend"]["service"]["name"])
        for r in s.get("rules", []):
            host(r.get("host", ""))
            for p in r.get("http", {}).get("paths", []):
                if "service" in p["backend"]:
                    svc(p["backend"]["service"]["name"])
        for t in s.get("tls", []):
            for h in t.get("hosts", []):
                host(h)
            if t.get("secretName"):
                secret(t["secretName"])


def anonymise(i):
    m = i["metadata"]
    out_meta = {"name": ing(m["name"]), "namespace": ns(m["namespace"])}
    a = {}
    for k, v in m.get("annotations", {}).items():
        if DROP_ANNOTATION.match(k):
            continue
        if k in ISSUER:
            v = issuer(v)
        elif k == RELEASE:
            v = release(v)
        elif k == RELEASE_NS:
            v = ns(v)
        else:
            v = text(v)
        a[k] = v
    if a:
        out_meta["annotations"] = a
    s = i["spec"]
    if "defaultBackend" in s:
        backend(s["defaultBackend"])
    for r in s.get("rules", []):
        if "host" in r:
            r["host"] = host(r["host"])
        for p in r.get("http", {}).get("paths", []):
            if "path" in p:
                p["path"] = text(p["path"])
            backend(p["backend"])
    for t in s.get("tls", []):
        if "hosts" in t:
            t["hosts"] = [host(h) for h in t["hosts"]]
        if t.get("secretName"):
            t["secretName"] = secret(t["secretName"])
    return {"apiVersion": "networking.k8s.io/v1", "kind": "Ingress",
            "metadata": out_meta, "spec": s}


def leftovers(out, items):
    words = set()
    for names in (ns, ing, svc, secret, issuer, release):
        words.update(n for n in names.map if len(n) > 3)
    for h in hosts:
        words.add(h)
        words.update(l for l in h.split(".") if len(l) > 3 and l not in COMMON)
    found = []
    for w in sorted(words):
        if re.search(r"(?<![A-Za-z0-9])%s(?![A-Za-z0-9])" % re.escape(w), out):
            found.append(w)
    return found


def main():
    with open(sys.argv[1]) as f:
        data = json.load(f)
    items = data["items"] if "items" in data else [data]
    first_pass(items)
    out = {"apiVersion": "v1", "kind": "List",
           "items": [anonymise(i) for i in items]}
    text_out = json.dumps(out, indent=2, ensure_ascii=False) + "\n"
    left = leftovers(text_out, items)
    if left:
        sys.stderr.write("names left in the output, nothing written: %d (run with -v to see them)\n" % len(left))
        if "-v" in sys.argv:
            sys.stderr.write("\n".join(left) + "\n")
        sys.exit(1)
    sys.stdout.write(text_out)


if __name__ == "__main__":
    main()
