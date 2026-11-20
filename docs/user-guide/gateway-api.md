# Gateway API

wxingress serves the [Gateway API](https://gateway-api.sigs.k8s.io/) next to Ingresses and NGINX Ingress
Controller's resources, from the same controller and the same NGINX. The Gateway API semantics
(attachment of routes to listeners, hostnames, ReferenceGrants, conflicts and every status condition) are
those of [NGINX Gateway Fabric](https://github.com/nginx/nginx-gateway-fabric) 2.8.1, whose graph
wxingress contains as a copy. The NGINX configuration is wxingress's own.

| | |
|---|---|
| Gateway API version | v1.6.3, standard channel |
| Routes | HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, UDPRoute |
| HTTPRoute extended features | method, query and port matching, response header modification, path and host rewrite, path and port redirect, and more |
| GatewayClass `controllerName` | `wxingress.wizardtales.com/gateway-controller` |

## Turning it on

```sh
helm upgrade wxingress oci://ghcr.io/wx-one/charts/wxingress -n wxingress --reuse-values \
  --set controller.gatewayAPI.enable=true \
  --set controller.gatewayAPI.installCRDs=true
```

| Value | Default | What it does |
|---|---|---|
| `controller.gatewayAPI.enable` | `false` | Serve the Gateway API. Its CRDs must exist. |
| `controller.gatewayAPI.installCRDs` | `false` | Install the standard channel CRDs of the matching Gateway API version, unless another release or tool owns them. They are kept on uninstall. |
| `controller.gatewayAPI.gatewayClass.name` | `wxingress` | The GatewayClass this release serves. |
| `controller.gatewayAPI.gatewayClass.create` | `true` | Create that GatewayClass. |
| `controller.gatewayAPI.defaultGateway.name` | `""` | A Gateway that shares the controller's address and ports 80/443 with Ingresses. Meant for migrations. |
| `controller.gatewayAPI.service.type` | `LoadBalancer` | The Service type of Gateways with an address of their own. |
| `controller.gatewayAPI.listenerPorts` | `20000-29999` | The ports NGINX uses inside the pod for those Gateways' listeners. |

## Addresses

Every Gateway gets an address of its own: the controller creates a Service for it in the release
namespace, owned by the GatewayClass and removed together with the Gateway.

The exception is the **default Gateway** (`controller.gatewayAPI.defaultGateway.name`). It shares the
controller's own Service and ports 80 and 443 with Ingresses and VirtualServers, so routes and Ingresses
can serve hosts from the same address during a migration. Its listeners can only be HTTP on 80 and HTTPS
on 443. This is a deliberate deviation from the Gateway API.

## A first route

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: web
  namespace: default
spec:
  gatewayClassName: wxingress
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: app
  namespace: default
spec:
  parentRefs:
    - name: web
  hostnames:
    - app.example.com
  rules:
    - backendRefs:
        - name: app
          port: 80
```

```sh
kubectl get gateway web -o jsonpath='{.status.addresses[0].value}'
```

## ingress-nginx annotations on routes

Routes accept many of the ingress-nginx annotations that tune an upstream, read by the same code as on an
Ingress: timeouts, body size, buffering, `proxy-next-upstream*`, `backend-protocol`, `load-balance`,
`upstream-hash-by`, `affinity` and `session-cookie-*`, rate limits, canary by header or cookie, and, with
snippets enabled, the snippet annotations. Rate limits, authentication and access control are NGINX
Ingress Controller's Policies, attached with `wxingress.wizardtales.com/policies: <policy>[, ...]` on
the route or the Gateway. Redirects, rewrites, header changes and CORS are the Gateway API's own filters.

The full list is in [ingress-nginx compatibility](ingress-nginx-compatibility.md#the-gateway-api-the-same-annotations-on-routes).

## Conformance

`dataplane/e2e/gateway-conformance/run.sh` runs the Gateway API conformance suite against wxingress on
kind. wxingress only claims a feature once its conformance tests pass.
