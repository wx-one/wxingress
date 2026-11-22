# Changelog

wxingress versions are `<NGINX Ingress Controller version>-wx<n>`: `5.7.1-wx5` is the fifth wxingress
release based on NGINX Ingress Controller 5.7.1. The changes of NGINX Ingress Controller itself are on its
[releases page](https://github.com/nginx/kubernetes-ingress/releases).

## 5.7.1-wx6

A security release. Upgrade from every earlier version.

### Security

- `auth-url` and `mirror-target` can no longer reach a unix socket of the pod. nginx takes `unix:` in
  `proxy_pass`, also when a variable is completed at request time, so a tenant who may write Ingresses
  could send requests to the data plane module's API and change the upstreams, certificates and canaries
  of other tenants. `auth-url` takes `http` or `https` with a DNS name or an address and an optional port,
  or `$host`/`$server_name` without one; `mirror-target` takes variables only in its path. JWKS and OIDC
  URLs of Policies refuse `unix:`.
- Ingresses of other namespaces no longer merge into a host, and canaries no longer attach to an Ingress
  of another namespace. ingress-nginx does both; set `-ingress-nginx-cross-namespace`
  (`controller.ingressNginxCrossNamespace`) where namespaces trust each other and rely on it.
- A server alias never takes a host from another Ingress, VirtualServer or TransportServer; wildcard
  aliases need at least two labels after `*.`.
- Annotation values that nginx would refuse no longer reach the configuration, where one Ingress broke
  the reload for all: variables only from a fixed list (`$1` to `$9`, `$http_*`, `$arg_*`, `$cookie_*`
  and the core variables), `proxy-redirect-from/to` checked like their nginx.org equivalents,
  `upstream-hash-by` without braces, `use-regex` paths without what PCRE refuses. No line breaks in CORS
  headers or `auth-proxy-set-headers` values.
- The admission webhook limits request bodies to 3.5 MiB and four reviews at once, and runs `nginx -t`
  on a copy with a timeout, outside the lock of the sync.
- Gateway API: the nginx port of a Gateway is only taken from a Service the controller made for it and
  only from the listener port range; routes compete for hosts with their own creation time, so they no
  longer take hosts from older Ingresses; listeners that verify client certificates are refused instead
  of being served without verification; Policies of another namespace need a ReferenceGrant;
  restrictive ingress-nginx annotations on routes answer 503 instead of being dropped.
- Log lines escape line breaks and control characters; a panic in a sync task is logged instead of
  ending the pod.

### Image and chart

- The image is built on the hardened Debian 13 runtime image of container.gov.de: no shell, no package
  manager, no nginx-agent. Binaries are read-only; ports are bound through a file capability.
  `kubectl exec ... sh` no longer works.
- The chart runs with a read-only root filesystem and the `RuntimeDefault` seccomp profile by default.
  The init container that fills `/etc/nginx` needs `NET_BIND_SERVICE` like the controller.
- preStop sleeps with the Kubernetes `sleep` action (1.30 and later) or `/nginx-ingress sleep`.
- The ClusterRole writes Services only with `gatewayAPI.enable`; the Service writes of the controller's
  own namespace moved to the Role.
- An optional NetworkPolicy for the webhook port (`controller.admissionWebhooks.networkPolicy`).
- Releases are signed with cosign and carry build provenance and SBOM attestations.

### Changes to note when upgrading

- Upstream names are `<namespace>_<name>_<host>_<service>_<port>` and Ingress configuration files
  `<namespace>_<name>.conf`; dashboards keyed on upstream names need the new names. Old files are
  removed at startup.
- Annotations with variables outside the list above are ignored with a warning, or make the locations
  answer 503 where ingress-nginx does so for invalid values (`auth-url`, FastCGI parameters).

## 5.7.1-wx5

The first public release.

- An endpoint of an EndpointSlice without a `ready` condition is served, as the Kubernetes API defines.
  Hand-written EndpointSlices for Services without a selector often have no conditions.
- Licence: EUPL-1.2 (the code from NGINX Ingress Controller and NGINX Gateway Fabric stays Apache-2.0).
- Go module `github.com/wx-one/wxingress`; images on `ghcr.io/wx-one/wxingress`, the chart on
  `oci://ghcr.io/wx-one/charts/wxingress`.
- The chart defaults to the wxingress image.

## 5.7.1-wx4

- Behind the PROXY protocol (`use-proxy-protocol` in an ingress-nginx ConfigMap), the client's address is
  the PROXY header's (`real_ip_header proxy_protocol`), as in ingress-nginx. Before, it was taken from
  `X-Forwarded-For`, so the load balancer's address appeared as every client's.
- A `real-ip-header` or `set-real-ip-from` set in the ConfigMap wins over the translation of
  ingress-nginx's keys.

## 5.7.1-wx3

- A path that a host of an Ingress lists twice (for example a host listed twice with `/`) is served from
  the first rule, as in ingress-nginx. Before, it produced two `location` blocks and NGINX refused the whole
  configuration.
- A pod is not Ready while NGINX refuses the configuration of the resources at startup.

## 5.7.1-wx2

- With `-ingress-nginx-defaults`, an Ingress backend whose port number or name no Service port has is
  matched against the Service ports' `targetPort`, as in ingress-nginx (port 80 to 5000, Ingress says 5000).

## 5.7.1-wx1

- NGINX 1.31.9 with the fixes of the NGINX security advisories of November 2026.
- The leader gives up its Lease when it stops, so the next pod leads within seconds instead of after the
  Lease's 30 s.
- The chart renders the leader election Lease, the Gateway API CRDs and the safe-upgrades
  ValidatingAdmissionPolicy only where none exists or where it is the release's own, and keeps the Lease
  on uninstall.
