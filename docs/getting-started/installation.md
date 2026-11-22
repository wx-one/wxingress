# Installation

wxingress is installed with its Helm chart. The chart and the image are published on the GitHub
Container Registry. There are no static manifests: the chart sets the RBAC, the image and the security
context.

| | |
|---|---|
| Chart | `oci://ghcr.io/wx-one/charts/wxingress` |
| Image | `ghcr.io/wx-one/wxingress:<version>`, for example `5.7.1-wx9`, signed with cosign |
| Kubernetes | 1.25 or newer (tested on 1.31 and 1.32) |
| Helm | 3.8 or newer (OCI support) |

## A new cluster

If no ingress controller runs in the cluster yet:

```sh
helm install wxingress oci://ghcr.io/wx-one/charts/wxingress \
  --namespace wxingress --create-namespace \
  --set controller.ingressNginxCompat=true \
  --set controller.ingressClass.setAsDefaultIngress=true
```

This gives you:

- a Deployment of one wxingress pod behind a Service of type `LoadBalancer`,
- the IngressClass `nginx`, marked as the cluster's default class,
- ingress-nginx's behaviour (`controller.ingressNginxCompat=true`, see below), so Helm charts written for
  ingress-nginx work unchanged,
- the CRDs of NGINX Ingress Controller (VirtualServer, Policy, TransportServer and others).

Wait for the Service's address:

```sh
kubectl -n wxingress get service wxingress-controller -w
```

Then create your [first Ingress](first-ingress.md).

!!! tip "Without `ingressNginxCompat`"
    Leave `controller.ingressNginxCompat` off to run wxingress as plain NGINX Ingress Controller, with
    NIC's defaults and the IngressClass controller `nginx.wx1.eu/ingress-controller`. The
    ingress-nginx annotations are still read, but NGINX's own defaults apply where an Ingress sets nothing.

## The compatibility profile

`controller.ingressNginxCompat=true` turns on what running in ingress-nginx's place takes:

| It sets | The effect |
|---|---|
| `-ingress-nginx-defaults` | ingress-nginx's defaults wherever no ConfigMap key or annotation is set: timeouts, buffering, retries, keepalive, the redirect code, HSTS, HTTP/2, TLS, upstream headers, WebSocket. |
| `-dynamic-upstreams` | Endpoints change without an NGINX reload, as with ingress-nginx's Lua balancer. |
| `-allow-empty-ingress-host` | Ingresses without a host and Ingresses with only `spec.defaultBackend` are served. |
| a preStop sleep of 5 s | The load balancer or kube-proxy stops sending traffic before NGINX stops. Kubernetes' own `sleep` action from 1.30, before that `/nginx-ingress sleep 5` (the image has no `sleep`). |

The full list of what changes with the profile is in
[ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md#defaults-ingress-nginx-defaults).

## On the nodes' ports (bare metal, RKE2, kind)

ingress-nginx is often installed as a DaemonSet that holds ports 80 and 443 of every node. To run
wxingress like that:

```yaml title="values.yaml"
controller:
  kind: daemonset
  ingressNginxCompat: true
  hostPort:
    enable: true
    http: 80
    https: 443
  dnsPolicy: ClusterFirstWithHostNet
  service:
    create: true
    type: ClusterIP
  # optional: restart pods only when you delete them, node by node
  strategy:
    type: OnDelete
```

```sh
helm install wxingress oci://ghcr.io/wx-one/charts/wxingress \
  --namespace wxingress --create-namespace -f values.yaml
```

!!! note "Shutdown of a pod"
    On the node's ports there is no Service that drains a pod. Set the ConfigMap key
    `worker-shutdown-timeout` (for example `10s`) so that NGINX closes long-lived connections such as
    WebSockets soon after a pod is told to stop, instead of after ingress-nginx's default of 240 s.

## Replacing ingress-nginx

If ingress-nginx is installed, do not install wxingress with `setAsDefaultIngress` and do not let it
create the IngressClass `nginx`: that class belongs to the ingress-nginx release until it is gone. Follow
the [migration guide](../migration/ingress-nginx.md) instead. It shows how both controllers serve the
same Ingresses at the same time and how to move traffic over safely.

## The Gateway API

To serve Gateways and routes as well:

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.gatewayAPI.enable=true \
  --set controller.gatewayAPI.installCRDs=true
```

`installCRDs` installs the Gateway API standard channel CRDs that match wxingress's version, but only
where no other release or tool owns them. See [Gateway API](../user-guide/gateway-api.md).

## Security

The image is built on the hardened Debian 13 runtime seed of container.gov.de: no shell, no package
manager, nothing but NGINX, the modules the templates load (njs, OpenTelemetry, the data plane module),
the controller and the shared libraries they need. The binaries, templates and njs scripts belong to
root and are read-only; the controller and NGINX run as uid 101 and write only to `/etc/nginx` (its
`nginx.conf`, `conf.d`, `stream-conf.d` and `secrets`), `/var/cache/nginx`, `/var/lib/nginx` and
`/tmp`.

The chart's defaults:

| Setting | Default |
|---|---|
| `controller.readOnlyRootFilesystem` | `true`: those directories are emptyDirs; an init container (`/nginx-ingress copy`) fills `/etc/nginx` from the image |
| security context | `runAsUser: 101`, `runAsNonRoot`, `allowPrivilegeEscalation: false`, seccomp `RuntimeDefault`, all capabilities dropped but `NET_BIND_SERVICE` |
| RBAC | a ClusterRole that reads and writes only status, cert-manager Certificates and DNSEndpoints (when on) and, with the Gateway API, the Services and EndpointSlices of Gateways; a Role for what the controller writes in its own namespace |

NGINX and `/nginx-ingress` carry the file capability `cap_net_bind_service`, so they bind ports 80 and
443 as uid 101 also where the runtime does not open low ports to everybody. Keep `NET_BIND_SERVICE` in
the capabilities of a custom `controller.securityContext` or `controller.initContainerSecurityContext`:
without it, the kernel refuses to run them.

The admission webhook (`controller.admissionWebhooks.enabled`) takes Ingresses from whoever reaches its
port. Its port is never a hostPort, but with `controller.hostNetwork` it listens on the nodes'
addresses, where no NetworkPolicy applies: keep hostNetwork off with the webhook. With
`controller.admissionWebhooks.networkPolicy.enabled` and the API server's addresses in
`controller.admissionWebhooks.networkPolicy.from`, a NetworkPolicy lets only the API server reach it
(every other port of the pods stays open):

```yaml title="values.yaml"
controller:
  admissionWebhooks:
    enabled: true
    networkPolicy:
      enabled: true
      from:
        - ipBlock:
            cidr: 10.0.0.0/24   # the control plane nodes
```

With the Gateway API's default Gateway, `allowedRoutes: {namespaces: {from: All}}` lets any namespace
attach routes to it; a route only gets a host that no older Ingress or VirtualServer serves, but a host
nobody serves yet goes to the first route that claims it. Use it only when every namespace is trusted.

## Upgrading

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.image.tag=<new version>
```

Helm does not upgrade CRDs. Apply the CRDs of the new version before you upgrade the chart:

```sh
kubectl apply --server-side -f https://raw.githubusercontent.com/wx-one/wxingress/<tag>/deploy/crds.yaml
```

## Releases and SBOMs

Every release on [GitHub](https://github.com/wx-one/wxingress/releases) carries:

| File | What it is |
|---|---|
| `wxingress-<version>-linux-amd64.tar.gz` | the controller binary and the data plane module, with the licences |
| `wxingress-<version>-linux-amd64.sha256` | its SHA-256 |
| `wxingress-<version>-linux-amd64.cdx.json` | the SBOM of the release (CycloneDX 1.6): the Go modules of the controller, the libcurl and meta runtime in the data plane module, and the NGINX it is built for |
| `wxingress-<version>-linux-amd64.image.cdx.json` | the SBOM of what the image adds to its base: the release, NGINX and its modules from nginx.org, the Debian packages its libraries are copied from, and the base image by digest (it carries a signed SBOM of its own) |
| `wxingress-<chart version>.tgz` | the Helm chart |

The release notes name the image by its digest, and the chart of a release deploys it by that digest.
To deploy exactly that image with another chart version:

```sh
helm upgrade --install wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress \
  --set controller.image.digest=sha256:<digest from the release notes>
```

The image is signed with cosign (keyless, by the release workflow) and has build provenance and SBOM
attestations in the registry:

```sh
cosign verify ghcr.io/wx-one/wxingress@sha256:<digest> \
  --certificate-identity-regexp '^https://github.com/wx-one/wxingress/.github/workflows/release.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/wx-one/wxingress@sha256:<digest> --repo wx-one/wxingress
```

The SBOMs are written from what the build knows, not by scanning: the controller's modules come from
its build info, the data plane's libraries from the pinned sources it is built from, and the image's
packages from the list the build writes into it (`/usr/share/doc/wxingress/PACKAGES`), from dpkg in
the build stage - the image itself has no package database.

## Uninstalling

```sh
helm uninstall wxingress -n wxingress
```

The leader election Lease and, if the chart installed them, the Gateway API CRDs are kept, so that an
uninstall never deletes Gateways or routes. The chart's notes say how to remove them.
