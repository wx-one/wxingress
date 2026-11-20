# Installation

wxingress is installed with its Helm chart. The chart is published as an OCI artifact on the GitHub
Container Registry, and the images are on Docker Hub.

| | |
|---|---|
| Chart | `oci://ghcr.io/wx-one/charts/wxingress` |
| Image | `ghcr.io/wx-one/wxingress:<version>`, for example `5.7.1-wx5` |
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
| a preStop `sleep 5` | The load balancer or kube-proxy stops sending traffic before NGINX stops. |

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

## Upgrading

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.image.tag=<new version>
```

Helm does not upgrade CRDs. Apply the CRDs of the new version before you upgrade the chart:

```sh
kubectl apply --server-side -f https://raw.githubusercontent.com/wx-one/wxingress/<tag>/deploy/crds.yaml
```

## Uninstalling

```sh
helm uninstall wxingress -n wxingress
```

The leader election Lease and, if the chart installed them, the Gateway API CRDs are kept, so that an
uninstall never deletes Gateways or routes. The chart's notes say how to remove them.
