# Changelog

wxingress versions are `<NGINX Ingress Controller version>-wx<n>`: `5.7.1-wx5` is the fifth wxingress
release based on NGINX Ingress Controller 5.7.1. The changes of NGINX Ingress Controller itself are on its
[releases page](https://github.com/nginx/kubernetes-ingress/releases).

## 5.8.1-wx1

Based on NGINX Ingress Controller 5.8.1 (from 5.7.1). Upgrade with the chart 2.9.0; nothing in the values,
the annotations or the ConfigMap needs to change, and the configuration NGINX gets for existing resources
is the same.

### New, from NGINX Ingress Controller 5.8

- The `connectionLimit` Policy: `limit_conn` for VirtualServers, VirtualServerRoutes and Ingresses.
- JWTAuth Policies validate the claims of a token.
- ExternalAuth Policies pass the headers of the auth response to the backend (`authResponseHeaders`).
- `nginx.org/proxy-set-headers` takes NGINX variables, from the same list VirtualServer headers allow.
- `-external-dns-group-version` (chart `controller.externalDNSGroupVersion`) for external-dns v0.21 and
  later.
- `-batch-reload-window` (chart `controller.batchReloadWindow`, 2 seconds): during a long run of endpoint
  changes, such as a rolling update, a configuration change waiting in a batch is applied after at most
  that long.
- Helm validates the values without network access.

### Changes

- An EndpointSlice no longer forces a reload at the end of a batch. A reload that is needed is kept and
  done; with `controller.dynamicUpstreams` the wxingress module takes the servers without one.
- A failed write after part of a batch was already written is still applied at the end of the batch.

## 5.7.1-wx13

Upgrade everywhere: NGINX 1.31.10 with the fixes of 23 November 2026.

### Fixes

- NGINX 1.31.10 (from 1.31.9) in the image and for the data plane module. It fixes CVE-2026-132779, where a
  client could make NGINX use excessive CPU when `ssl_early_data` is on; wxingress does not set it, only a
  snippet can. It also fixes NGINX hogging a CPU with TLS connections to backends, which wxingress opens for
  `backend-protocol: HTTPS` and for Policies with TLS to the upstream.
- The Gateway API graph follows NGINX Gateway Fabric 2.9.1 (from 2.9.0), which changes nothing in it that
  wxingress uses. The Gateway API stays v1.7.0.

## 5.7.1-wx12

Upgrade where wxingress runs on the nodes' ports behind a ClusterIP Service, as rke2-ingress-nginx does,
and something waits on the address in an Ingress's status, such as Argo CD.

### Fixes

- Ingresses get the addresses of the nodes wxingress's pods run on in their status, as with ingress-nginx
  without `--publish-service`, where no LoadBalancer Service, IngressLink or `external-status-address`
  gives one. Before, a DaemonSet on the nodes' ports behind a ClusterIP Service wrote no address, and
  Argo CD kept every application with an Ingress Progressing. The list follows the pods as nodes come and
  go; a pod that restarts keeps its node in it. With `controller.ingressNginxCompat` the chart passes its
  Service to the controller (`-external-service`) for a ClusterIP Service too, from which the nodes are
  found.
- `external-status-address` removed from the ConfigMap gives the status back to the Service or the nodes;
  before, the old address stayed until the pod restarted.

## 5.7.1-wx11

ListenerSets of the Gateway API, Gateway API v1.7.0. Upgrade where `load-balance` or `upstream-hash-by`
changes on routes with the wxingress module.

### Features

- ListenerSets of the Gateway API. A namespace adds listeners to a Gateway of another namespace, with
  certificates of its own, where the Gateway allows it in `spec.allowedListeners`. On the default
  Gateway this gives teams their own hostnames and certificates on the shared address, as with
  Ingresses, without access to the Gateway; cert-manager 1.20 or later issues the certificates from an
  annotation on the ListenerSet. Listeners of the Gateway keep their hostnames over those of a
  ListenerSet, and on the default Gateway a ListenerSet can only add HTTP on 80 and HTTPS on 443. The
  controller looks for the ListenerSet CRD when it starts and serves none without it. The chart's
  ClusterRole reads ListenerSets and writes their status.

### Changes

- The Gateway API is v1.7.0, and its semantics those of NGINX Gateway Fabric 2.9.0 (from 2.8.1).
  `controller.gatewayAPI.installCRDs` installs the CRDs of v1.7.0. Among NGF's fixes since 2.8.1: a
  listener of a ListenerSet with the name of a listener of its Gateway no longer takes the routes
  attached to the Gateway without a `sectionName` off it.

### Fixes

- With `-dynamic-upstreams`, changing `load-balance` or `upstream-hash-by` on a route reloaded NGINX
  since 5.7.1-wx10, which gave the route's upstream `least_time` for `ewma`. The wxingress module
  balances routes by its rule, as Ingresses: the upstream keeps NGINX's method, and the change
  reloads nothing.

## 5.7.1-wx10

A security release. Upgrade where an ingress-nginx ConfigMap has global allow or deny lists and
snippets are enabled.

### Security

- The global allow and deny lists of an ingress-nginx ConfigMap (`allowlist-source-range`,
  `whitelist-source-range`, `denylist-source-range`, `block-cidrs`, with `-ingress-nginx-defaults`) were
  rendered into the ConfigMap's `server-snippets`. With snippets enabled, an Ingress or VirtualServer
  with server snippets of its own replaced those, and its host was served without the global lists.
  They are now rules of every server of their own, which snippets do not replace. A range that is
  not a valid address or CIDR denies every request instead of failing the reload.

### Changes

- Routes of the Gateway API keep `least_time`, which open-source NGINX has: `load-balance: ewma`
  without the wxingress module is `least_time last_byte` on a route as on an Ingress, not round robin.

## 5.7.1-wx9

A security release. Upgrade from every earlier version; it matters most where JWT or OIDC Policies
are used.

### Security

- A JWT nobody verified could end an NGINX worker: deeply nested array claims were written out
  recursively, and claims are also read for refused requests (`add_header ... always`, log formats).
  Arrays are written out at most 8 levels deep.
- A key set read from a file could end up in another Policy's slot when all slots of a worker were in
  use, so that Policy accepted tokens signed with the wrong keys.
- An OIDC session that could not be remembered (a client secret of 512 bytes or more) left the session
  before it remembered, so the next request with that cookie got another user's tokens and claims.
- JWT parts with a top-level name given twice or a `\u0000` in a string are refused.
- Built with metalang 44cb41f0, which fixes in its runtime:
  - HTTP client: only `http` and `https` (a JWKS or OIDC endpoint from a provider's discovery
    document could name `file://` or `gopher://`), no line breaks in header names, values or the
    method, a 64 MiB limit and a total timeout for buffered bodies, retries that start empty.
  - HTTP: an unread request body is discarded before every answer; a request that ends while a JWT
    or OIDC check waits no longer leaves freed memory behind or keeps an old worker from exiting
    after a reload; routes match the normalised URI; line breaks in response headers and variables
    (an OIDC claim passed to `proxy_set_header`) are refused; all request headers are visible, not
    only the first 32.
  - Tasks: a 64 KiB guard below every task stack and stack clash protection; the offload pool works
    in workers forked from a master that used it; running out of memory or threads fails a request
    instead of ending the process.

## 5.7.1-wx8

- The data plane reads JWT key sets and OIDC session keys from their files off the event loop. Before,
  `stat`, `fopen` and `fread` ran in the request's access phase and stopped the worker while the file
  was read; a rotated OIDC session key is now looked for once a second instead of on every request.
- CI shows the analyses of the metalang compiler on every build and runs the certificates, JWT and OIDC
  tests of the data plane.

## 5.7.1-wx7

A security release for the data plane module. It matters where JWT or OIDC Policies are used; upgrade
from every earlier version.

### Security

- JWT and OIDC are checked on requests that NGINX redirects internally: a `rewrite` that changes the URI
  (VirtualServer routes with `matches`, `splits` or `rewritePath`), `try_files`, `error_page` and named
  locations. Before, those requests were served without a token. Built with metalang's fix for guards
  on internal redirects.
- OIDC sessions are bound to their Policy, issuer, client and host, and checked on every request; a
  session of one Policy is not accepted by another. Existing sessions end, users log in once more.
- JWKS, discovery and token endpoints are verified with TLS by default: `sslVerify` defaults to `true`
  for JWT and OIDC Policies (Policies that set `false` keep it). Key sets and providers are cached per
  TLS setting.
- One fetch per key set at a time, failed fetches are cached for 10 seconds.
- A refresh no longer redirects to another site (`//host`).
- Cookies are kept per request; a session can no longer pass to a concurrent request.
- Back-channel revocations are scoped to issuer and client; front-channel logout ends only the browser's
  own session; a full revocation table is refused instead of dropping entries.
- The module's API takes requests only with a token the controller makes at startup.
- The upstream listing of the module's API can no longer overflow its buffer.
- A writer that dies while holding a shared table no longer leaves readers spinning.

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
