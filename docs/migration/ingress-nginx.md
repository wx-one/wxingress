# Migrating from ingress-nginx

wxingress is a drop-in replacement for ingress-nginx. Your Ingresses, their `nginx.ingress.kubernetes.io/*`
annotations, the IngressClass `nginx` and the keys of your ingress-nginx ConfigMap stay as they are. The
one thing that does not carry over is **Lua in snippets**: rewrite that in njs, as described in
[Lua to njs](lua-to-njs.md).

This page takes you through the switch:

1. [Check](#1-check-what-your-ingresses-use) what your Ingresses use.
2. [Translate](#2-translate-your-values) your Helm values.
3. [Install wxingress beside ingress-nginx](#3-install-beside-ingress-nginx), serving the same Ingresses.
4. [Compare](#4-compare-the-answers) the answers of both.
5. [Move the traffic](#5-move-the-traffic).
6. [Hand over](#6-hand-over-the-admission-webhook) the admission webhook.
7. [Finish](#7-finish): take over the IngressClass and remove ingress-nginx.

Both controllers serve the same Ingresses during the whole switch, so every step can be undone.

## What stays the same

| ingress-nginx | wxingress |
|---|---|
| IngressClass with controller `k8s.io/ingress-nginx` | served (`controller.ingressClass.controllers`) |
| Ingresses without a class | served (`controller.watchIngressWithoutClass`, default on) |
| `nginx.ingress.kubernetes.io/*` annotations | read; see the [annotation reference](../reference/annotations.md) |
| ConfigMap keys | read and translated; see the [ConfigMap reference](../reference/configmap.md) |
| defaults (5 s connect timeout, buffering off, 3 retries, HSTS, …) | the same with `controller.ingressNginxCompat=true` |
| endpoints change without a reload | the same (`-dynamic-upstreams`, a data plane module instead of Lua) |
| admission webhook | refuses what ingress-nginx's refuses, and what NGINX would not load |
| Prometheus metrics `nginx_ingress_controller_*` | exported under the same names and labels |
| `--tcp-services-configmap`, `--udp-services-configmap`, `--default-backend-service` | supported |
| several Ingresses for one host | merged into one server, as in ingress-nginx |
| a host listed twice in an Ingress | merged; the first rule of a path wins, as in ingress-nginx |

## 1. Check what your Ingresses use

List the annotations in use:

```sh
kubectl get ingress -A -o json \
  | jq -r '.items[].metadata.annotations // {} | keys[] | select(startswith("nginx.ingress.kubernetes.io/"))' \
  | sort | uniq -c
```

Compare the list with the [annotation reference](../reference/annotations.md). After the install,
wxingress puts a Warning event on every Ingress that uses an annotation it does not support. If the
annotation restricts access (ModSecurity, global auth and the like), it also logs an error saying that
the Ingress is served without it.

Then look for these, which need attention:

**Lua in snippets.** Find snippets with Lua code:

```sh
kubectl get ingress -A -o json | jq -r '.items[]
  | "\(.metadata.namespace)/\(.metadata.name)" as $n
  | .metadata.annotations // {} | to_entries[]
  | select((.key | test("snippet")) and (.value | test("_by_lua|lua_|ngx\\.")))
  | "\($n) \(.key)"'
```

Each hit needs an njs version: see [Lua to njs](lua-to-njs.md). Snippets that contain only NGINX
directives work as they are once snippets are enabled (`controller.enableSnippets=true`, the equivalent
of ingress-nginx's `allow-snippet-annotations: "true"`).

**NetworkPolicies that let only ingress-nginx in.** A policy that allows traffic from pods labelled
`app.kubernetes.io/name: ingress-nginx` (or `rke2-ingress-nginx`) blocks wxingress's pods, and requests
to those backends time out. Find them:

```sh
kubectl get netpol -A -o json | jq -r '.items[]
  | select(tostring | test("ingress-nginx"))
  | "\(.metadata.namespace)/\(.metadata.name)"'
```

Add a second policy next to each one, with the same `podSelector` and ports, that allows traffic from the
namespace of wxingress (`kubernetes.io/metadata.name: wxingress`) and pods labelled
`app.kubernetes.io/instance: wxingress`.

**Taints.** wxingress pods tolerate only the taints you configure. If ingress-nginx pods run on tainted
nodes, give wxingress the same tolerations. Remember that a pod which started before a taint was added
keeps running, but a new pod is not placed there.

**ConfigMap keys that do nothing today.** Some keys in old ConfigMaps never took effect in your
ingress-nginx version. For example, `block-user-agents` renders nothing in ingress-nginx 1.14. wxingress
does not take that key; you can get the same 403 with a ConfigMap snippet (see
[ConfigMap](../reference/configmap.md)). Decide whether you want the behaviour you configured or the
behaviour you had.

## 2. Translate your values

wxingress has its own Helm chart, `charts/wxingress`. It renders NGINX Ingress Controller's flags plus
wxingress's own, so ingress-nginx's chart values do not apply directly. The translator in the
repository converts a release's values once:

```sh
git clone https://github.com/wx-one/wxingress && cd wxingress

# a Helm release of ingress-nginx
helm get values -n ingress-nginx ingress-nginx -o yaml \
  | go run ./hack/ingress-nginx-values > wxingress-values.yaml

# RKE2's bundled rke2-ingress-nginx: its HelmChartConfig, and RKE2's defaults under it
kubectl -n kube-system get helmchartconfig rke2-ingress-nginx -o yaml \
  | go run ./hack/ingress-nginx-values -rke2 > wxingress-values.yaml
```

It carries over the kind, hostPort, dnsPolicy, node selection, tolerations, resources, the Service, the
class, the ConfigMap keys, metrics, a PodDisruptionBudget, `tcp`, `udp` and the default backend. On
stderr it prints a line for everything it could not carry over or that needs a decision. **Read those
lines and the resulting values before you install.**

The values set `controller.ingressNginxCompat: true`, the
[compatibility profile](../getting-started/installation.md#the-compatibility-profile).

## 3. Install beside ingress-nginx

While ingress-nginx still runs, install wxingress so that it serves the same Ingresses without taking
anything away from ingress-nginx:

```yaml title="additions to wxingress-values.yaml"
controller:
  ingressNginxCompat: true
  ingressClass:
    name: nginx
    create: false              # the IngressClass belongs to the ingress-nginx release
    controllers:
      - k8s.io/ingress-nginx   # serve ingress-nginx's class
  reportIngressStatus:
    enable: false              # one writer of .status at a time
  admissionWebhooks:
    failurePolicy: Ignore      # made Fail in step 6
```

```sh
helm install wxingress oci://ghcr.io/wx-one/charts/wxingress \
  --namespace wxingress --create-namespace -f wxingress-values.yaml
```

If ingress-nginx holds the nodes' ports 80 and 443 (hostPort or hostNetwork), wxingress cannot run on
the same node. Install it with a node affinity that keeps it off every node for now; the
[RKE2 runbook](rke2.md) shows how and also applies to plain ingress-nginx DaemonSets.

## 4. Compare the answers

Before any traffic moves, send the same requests to both controllers and compare them. Through
wxingress's Service address, or, for a DaemonSet, through a pod of the same chart without hostPort:

```sh
kubectl get ingress -A -o jsonpath='{range .items[*]}{range .spec.rules[*]}{.host}{"\n"}{end}{end}' \
  | sort -u | grep -v '^\*' > hosts.txt

probe() { # probe <address>: the status and size of / for every host
  while read -r h; do
    printf '%s ' "$h"
    curl -sk -o /dev/null -w '%{http_code} %{size_download}\n' --max-time 10 \
      --resolve "$h:443:$1" "https://$h/"
  done < hosts.txt
}
probe <ingress-nginx address> > before.txt
probe <wxingress address> > after.txt
diff <(cut -d' ' -f1,2 before.txt) <(cut -d' ' -f1,2 after.txt)
```

Wait about half a minute after a wxingress pod becomes Ready before you probe it, so that it has
received every resource. Then read its log for warnings:

```sh
kubectl -n wxingress logs deploy/wxingress-controller | grep -E '^[WE][0-9]'
```

A few differences are expected; see [Known differences](#known-differences) below.

## 5. Move the traffic

How depends on how traffic reaches the controllers:

- **Behind a LoadBalancer Service or an external load balancer:** move the DNS name or the load balancer's
  backends from ingress-nginx's address to wxingress's. To go back, move them back.
- **On the nodes' ports (hostPort, hostNetwork, RKE2):** only one controller can hold port 80 of a node.
  A node label decides which one runs where, and the nodes move over one at a time. Follow the
  [RKE2 runbook](rke2.md).

!!! warning "Shutdown of ingress-nginx pods"
    ingress-nginx's default `worker-shutdown-timeout` is 240 s. A pod that is stopped keeps the node's
    ports for up to four minutes while it waits for open WebSockets. Set `worker-shutdown-timeout: "10s"`
    in ingress-nginx's ConfigMap before you start, and do not delete pods with `--force`: on a hostPort
    node, the old pod's port forwarding rule then stays in place and the node answers nothing.

## 6. Hand over the admission webhook

ingress-nginx's admission webhook checks every Ingress create and update, usually with
`failurePolicy: Fail`. Once the last ingress-nginx pod is gone, nothing answers it, and every Ingress
change in the cluster fails, including cert-manager's HTTP-01 solvers and Helm upgrades.

So **before the last ingress-nginx pod goes**:

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.admissionWebhooks.failurePolicy=Fail

# every Ingress passes wxingress's webhook as it is
kubectl get ingress -A -o json | jq 'del(.items[].metadata.resourceVersion)' \
  | kubectl apply --dry-run=server -f - 2>&1 | grep -c 'denied the request'   # 0
```

Then remove ingress-nginx's webhook through its chart (`controller.admissionWebhooks.enabled=false`), so
that a later upgrade of that chart does not bring it back.

!!! note
    Ingresses with snippet annotations are refused by wxingress's webhook while snippets are disabled,
    just as ingress-nginx refuses them. If you replaced a Lua snippet with njs in the ConfigMap, remove the
    old snippet annotation from the Ingress's source, or its next deployment will be refused.

## 7. Finish

When ingress-nginx serves nothing any more:

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.ingressClass.create=true \
  --set controller.reportIngressStatus.enable=true
```

Then uninstall ingress-nginx. On RKE2, disable the bundled chart (`disable: [rke2-ingress-nginx]` in
`/etc/rancher/rke2/config.yaml`, or in the cluster's configuration in Rancher) so that an upgrade of RKE2
does not reinstall it. On RKE1, set the ingress provider to `none`.

## Known differences

These are the differences seen in production switches and in the [test corpus](testing.md). None of them
changes what a working route serves.

| Situation | ingress-nginx | wxingress |
|---|---|---|
| Lua in a snippet | runs | not supported; use [njs](lua-to-njs.md) |
| A path no Ingress of the host defines, on a host whose Ingress has auth or an allow list | the Ingress's auth or allow list answers (401/403) | 404 |
| A wildcard host `*.example.com` | matches one label (`a.example.com`) | matches any depth (`a.b.example.com`), as NGINX's `server_name` does |
| A Service that does not exist | 503 | 502 |
| A Service without endpoints, with `--default-backend-service` set | the default backend (often 404) | 503 |
| An EndpointSlice endpoint without a `ready` condition (hand-written slices) | not served (1.12) | served, as the Kubernetes API defines |
| `block-user-agents` in the ConfigMap | nothing in 1.14 | not taken; use a ConfigMap snippet |
| A 426 answer of a backend | without HSTS | with HSTS |

The test corpus run lists 51 more allowed differences, each with its reason, in
`dataplane/e2e/ingress-nginx-parity.allow`.

## Commercial help

If you would like help with a switch, WizardTales offers [support packages](../about/support.md).
