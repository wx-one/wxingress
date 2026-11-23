# Gateway API

wxingress serves the [Gateway API](https://gateway-api.sigs.k8s.io/) next to Ingresses and NGINX Ingress
Controller's resources, from the same controller and the same NGINX. The Gateway API semantics
(attachment of routes to listeners, hostnames, ReferenceGrants, conflicts and every status condition) are
those of [NGINX Gateway Fabric](https://github.com/nginx/nginx-gateway-fabric) 2.9.0, whose graph
wxingress contains as a copy. The NGINX configuration is wxingress's own.

!!! info "Status"
    The Gateway API support is tested: the conformance suite of the Gateway API and wxingress's own
    end-to-end tests run against it. Unlike the Ingress side, it does not run in our production clusters
    yet. Feedback from real use is very welcome.

| | |
|---|---|
| Gateway API version | v1.7.0, standard channel |
| Routes | HTTPRoute, GRPCRoute, TLSRoute, TCPRoute, UDPRoute |
| ListenerSet | listeners and certificates of other namespaces, on the default Gateway too ([below](#listeners-of-other-namespaces-listenersets)) |
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

Every Gateway gets an address of its own: the controller creates a Service for it in the Gateway's
namespace, owned by the Gateway and removed together with it. The Service maps each listener port onto a
port NGINX listens on for this Gateway alone, from `controller.gatewayAPI.listenerPorts`.

Whoever can edit Services in the Gateway's namespace can edit that Service. The controller therefore
checks the NGINX ports it reads back: a port is only served if the Service is owned by the Gateway (its
controlling owner reference, with the Gateway's UID), the port lies within `listenerPorts`, is none of
the ports NGINX or the controller use otherwise, and no other listener has it. Where two listeners claim
the same port, the Service that was written to first keeps it. A port that fails a check is not served,
and the controller puts a free port back into the Service. A Service with the Gateway's name that the
controller did not create for this Gateway is never taken over: the Gateway stays without an address
(`Programmed: False`, `AddressNotAssigned`) until it is deleted.

`spec.infrastructure.labels` and `spec.infrastructure.annotations` of a Gateway are copied onto its
Service, as the Gateway API intends. With a Service of type `LoadBalancer` these annotations reach the
cloud provider's load balancer controller, so the owners of a Gateway can choose, for example, an
internal load balancer, a fixed or pre-allocated IP address, or source ranges, as far as the provider
reads them from annotations. If that must not be up to them, restrict who may create Gateways, or limit
the annotations with an admission policy.

The exception is the **default Gateway** (`controller.gatewayAPI.defaultGateway.name`). It shares the
controller's own Service and ports 80 and 443 with Ingresses and VirtualServers, so routes and Ingresses
can serve hosts from the same address during a migration. Its listeners can only be HTTP on 80 and HTTPS
on 443. This is a deliberate deviation from the Gateway API.

On the default Gateway a host can be served by a route and by an Ingress or VirtualServer at once; as
between Ingresses, the oldest one gets it. A route's host counts as created when the first route that
brought that hostname was created, but never before the Gateway itself. Hosts that existing Ingresses
and VirtualServers already serve therefore stay with them, even when the default Gateway takes routes
from all namespaces.

## Listeners of other namespaces: ListenerSets

On a Gateway, certificates belong to listeners, not to routes: an HTTPRoute cannot name one. With an
Ingress, every team brings its certificate along in `spec.tls`; with a Gateway, someone who may edit the
Gateway would have to add a listener and its certificate for every new hostname.

A [ListenerSet](https://gateway-api.sigs.k8s.io/guides/listener-set/) lets a namespace add listeners to a
Gateway of another namespace, with certificates of its own namespace, without access to the Gateway.
The Gateway decides which namespaces may do that in `spec.allowedListeners`. This also works on the
default Gateway, so teams can serve their own hostnames with their own certificates from the shared
address, as they do with Ingresses:

```yaml
# once, on the default Gateway
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: default
  namespace: wxingress
spec:
  gatewayClassName: wxingress
  allowedListeners:
    namespaces:
      from: Selector
      selector:
        matchLabels:
          wxingress.wizardtales.com/default-gateway: allowed
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Selector
          selector:
            matchLabels:
              wxingress.wizardtales.com/default-gateway: allowed
---
# in the team's namespace, which has the label
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata:
  name: app
  namespace: team-a
spec:
  parentRef:
    name: default
    namespace: wxingress
  listeners:
    - name: https
      hostname: app.example.com
      protocol: HTTPS
      port: 443
      tls:
        mode: Terminate
        certificateRefs:
          - name: app-tls          # a Secret of team-a
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: app
  namespace: team-a
spec:
  parentRefs:
    - group: gateway.networking.k8s.io
      kind: ListenerSet
      name: app
      sectionName: https
  hostnames: ["app.example.com"]
  rules:
    - backendRefs:
        - name: app
          port: 80
```

What a ListenerSet may do is bounded:

- On the default Gateway its listeners can only be HTTP on 80 and HTTPS on 443, as the Gateway's own.
  A listener on another port is not accepted (`PortUnavailable`).
- A listener on a port and hostname a listener of the Gateway already has is a conflict
  (`Conflicted`): the Gateway's listener and its certificate keep the hostname. Between two
  ListenerSets, the older one keeps it.
- A host that an Ingress or VirtualServer already serves stays with it, as for any route on the default
  Gateway.
- A listener name may repeat a name of the Gateway's listeners. Routes attached to the Gateway stay on
  the Gateway's own listeners.

The controller looks for the ListenerSet CRD when it starts. It is part of the standard channel since
Gateway API 1.5, and `controller.gatewayAPI.installCRDs` installs it. Without the CRD, ListenerSets are not
served and the controller logs a warning; after installing the CRD, restart the controller.

### Certificates from cert-manager

cert-manager 1.20 or later creates the certificates of a ListenerSet's listeners from an annotation, as it
does for Ingresses, with its Gateway API support and the ListenerSet feature turned on:

```yaml
# values of the cert-manager chart
config:
  apiVersion: controller.config.cert-manager.io/v1alpha1
  kind: ControllerConfiguration
  gatewayAPI:
    enabled: true
    enableListenerSet: true
  featureGates:
    ListenerSets: true
```

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: ListenerSet
metadata:
  name: app
  namespace: team-a
  annotations:
    cert-manager.io/issuer: letsencrypt   # or cert-manager.io/cluster-issuer
```

cert-manager then creates a Certificate for every listener with a hostname, in the ListenerSet's
namespace, with the Secret the listener names.

For HTTP-01, let the issuer answer the challenge with an HTTPRoute on the HTTP listener of the default
Gateway, not with an Ingress: on the default Gateway the older of a route and an Ingress keeps a host, and
an Ingress for the challenge would come after the routes of that host.

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: letsencrypt
  namespace: team-a
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: admin@example.com
    privateKeySecretRef:
      name: letsencrypt-account-key
    solvers:
      - http01:
          gatewayHTTPRoute:
            parentRefs:
              - group: gateway.networking.k8s.io
                kind: Gateway
                name: default
                namespace: wxingress
                sectionName: http
```

## Client certificates

Validation of client certificates on a listener (`spec.tls.frontend` of the Gateway) is not supported.
A listener that asks for it is not served: it gets `Accepted: False` with the reason `UnsupportedValue`
and `Programmed: False`, rather than being served to clients without verification. Use an `ingressMTLS`
Policy, attached with `wxingress.wizardtales.com/policies`, to verify client certificates.

## Status messages

A Gateway's status is readable by everyone who may read the Gateway, and a Gateway takes routes from
other namespaces. Where the configuration made from it is rejected, conflicts with another resource or
cannot be applied, its `Programmed` condition only names the affected hostname and listener port. The
full message is in the controller's log.

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
the route or the Gateway; `jwt` and `oidc` work without NGINX Plus (see [JWT and OIDC](jwt-oidc.md)). Redirects, rewrites, header changes and CORS are the Gateway API's own filters.

A route or Gateway may name any Policy of its own namespace. A Policy of another namespace
(`<namespace>/<name>`) needs the consent of that namespace: a ReferenceGrant there that allows it
`from` the route's kind (`HTTPRoute`, `GRPCRoute`) or `Gateway` in the route's namespace, `to` group
`k8s.nginx.org`, kind `Policy`, with the Policy's name or without one for all Policies:

```yaml
apiVersion: gateway.networking.k8s.io/v1beta1
kind: ReferenceGrant
metadata:
  name: shared-policies
  namespace: security
spec:
  from:
    - group: gateway.networking.k8s.io
      kind: HTTPRoute
      namespace: default
  to:
    - group: k8s.nginx.org
      kind: Policy
      name: corporate-jwt
```

Without one the Policy is not applied, and the rules of the route answer 500, as for a Policy that does
not exist; a Gateway that names such a Policy answers 500 for all its hostnames.

The ingress-nginx annotations that restrict who gets an answer are not taken on routes: `auth-*`,
`allowlist-source-range`/`whitelist-source-range`, `denylist-source-range`, `satisfy`,
`enable-modsecurity` and `enable-owasp-core-rules` (other than `false`), `modsecurity-snippet` and
`global-rate-limit` (other than 0). Rather than serve a route without the restriction, its rules answer 503 and the
controller logs a warning. Use the equivalent Policy instead.

The full list is in [ingress-nginx compatibility](ingress-nginx-compatibility.md#the-gateway-api-the-same-annotations-on-routes).

## Conformance

`dataplane/e2e/gateway-conformance/run.sh` runs the Gateway API conformance suite against wxingress on
kind. wxingress only claims a feature once its conformance tests pass.
