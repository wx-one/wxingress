# Architecture

This page describes how wxingress is built, for developers working on the code. wxingress has three
parts:

- **the controller** (Go), a fork of [NGINX Ingress Controller](https://github.com/nginx/kubernetes-ingress)
  5.8.1. It watches the Kubernetes API, renders the NGINX configuration and reloads NGINX. On top of the
  upstream controller it reads Ingresses with the semantics of ingress-nginx;
- **the Gateway API layer** (Go, `internal/gateway/`), built on the Gateway API graph of
  [NGINX Gateway Fabric](https://github.com/nginx/nginx-gateway-fabric). It maps Gateways and their
  routes onto VirtualServers and TransportServers, which then take the controller's normal path;
- **the data plane module** (`dataplane/`, metalang), loaded into NGINX. It takes what changes often -
  endpoints, canaries, balancing rules, certificates - over an API instead of a reload, counts the
  ingress-nginx request metrics, and implements the `jwt` and `oidc` Policies without NGINX Plus.

```text
                    Kubernetes API
   Ingress  VirtualServer  TransportServer  Policy    Gateway  HTTPRoute ...
      │            │              │            │          │          │
      ▼            ▼              ▼            ▼          ▼          ▼
 ┌──────────────────────────────────────────────┐  ┌───────────────────────┐
 │ controller            internal/k8s           │  │ Gateway API layer     │
 │  informers → queue → sync                    │◄─┤ internal/gateway      │
 │  ingress-nginx shape, classes, services      │  │ NGF graph → Virtual-  │
 │  secret store, status, leader election       │  │ Servers, Transport-   │
 │                                              │  │ Servers, status       │
 │ config generation     internal/configs       │  └───────────────────────┘
 │  annotations (nginx.org + ingress-nginx)     │
 │  version1 / version2 templates               │
 └───────────┬───────────────────────┬──────────┘
             │ nginx.conf + reload   │ upstream API, wxingress API (unix socket)
             ▼                       ▼
 ┌──────────────────────────────────────────────┐
 │ NGINX                                        │
 │  ngx_http_wxingress_module (dataplane/)      │
 │  servers, canaries, balancers, certificates, │
 │  metrics, JWT, OIDC in shared memory         │
 └──────────────────────────────────────────────┘
```

What wxingress takes from each upstream project, and how upstream releases are merged, is described in
[Upstream](upstream.md).

## How a Kubernetes controller works

A Kubernetes controller follows a **watch → queue → reconcile** loop:

1. **Informers** establish a watch against the Kubernetes API server and maintain a local in-memory cache
   of resources (Ingress, VirtualServer, Secret, ...).
2. When a resource changes, the informer fires an **event handler** (Add, Update, Delete), which places a
   lightweight **task** on a work queue.
3. A **sync loop** takes tasks off the queue one at a time and reconciles the desired state (what the
   user declared) with the actual state (what NGINX is running).

The controller uses raw [client-go](https://pkg.go.dev/k8s.io/client-go) with `SharedInformerFactory` and
a single work queue. It does **not** use controller-runtime or the Operator SDK.

---

## Layers

```text
┌──────────────────────────────────────────────────────────┐
│  Data model          pkg/apis/configuration/v1/          │
│  (CRD structs)       types.go, zz_generated.deepcopy.go  │
├──────────────────────────────────────────────────────────┤
│  Validation          pkg/apis/configuration/validation/  │
│                      internal/k8s/validation.go          │
│                      internal/k8s/admission*.go          │
├──────────────────────────────────────────────────────────┤
│  Controller          internal/k8s/                       │
│  (event loop)        controller.go, handlers.go,         │
│                      configuration.go, secrets/, status, │
│                      ingress_*.go, gateway_api.go,       │
│                      dynamic.go                          │
├──────────────────────────────────────────────────────────┤
│  Gateway API         internal/gateway/                   │
│                      internal/third_party/ngf/ (copy)    │
├──────────────────────────────────────────────────────────┤
│  Config generation   internal/configs/                   │
│                      configurator.go, virtualserver.go,  │
│                      ingress.go, ingress_nginx*.go,      │
│                      policy.go, dynamic.go, version1/,   │
│                      version2/                           │
├──────────────────────────────────────────────────────────┤
│  Process management  internal/nginx/                     │
│                      manager.go, rollback_manager.go     │
├──────────────────────────────────────────────────────────┤
│  Data plane          dataplane/src/ (in NGINX)           │
└──────────────────────────────────────────────────────────┘
```

| Layer | Package(s) | Responsibility |
| --- | --- | --- |
| Data model | `pkg/apis/configuration/v1/` | CRD Go structs with kubebuilder markers, generated DeepCopy. The source of truth for the API. |
| Validation | `pkg/apis/configuration/validation/`, `internal/k8s/validation.go`, `internal/k8s/admission*.go` | Field validation of VirtualServer, Policy and TransportServer, annotation validation of Ingresses, and the admission webhook. Returns `field.ErrorList`. |
| Controller | `internal/k8s/` | Informers, event handlers, sync dispatch, in-memory state, which Ingresses are served and in which shape, secret resolution, status, leader election, what is applied without a reload. |
| Gateway API | `internal/gateway/`, `internal/third_party/ngf/` | Builds NGF's graph from listers, maps it onto VirtualServers and TransportServers, creates the Services of Gateways, writes the Gateway API status. |
| Config generation | `internal/configs/`, `version1/`, `version2/` | Turns extended resources (`IngressEx`, `VirtualServerEx`, `TransportServerEx`) into config structs, parses annotations and ConfigMap keys of NGINX Ingress Controller and ingress-nginx, renders templates, decides what needs a reload. |
| Process management | `internal/nginx/` | Starts, reloads and stops NGINX, validates with `nginx -t`, rolls back failed configurations, talks to the module's API. |
| Data plane | `dataplane/src/` | Runs inside NGINX; see [The data plane module](#the-data-plane-module). |

### Layer crossing rules

- **Config generation must not call the Kubernetes API or the secret store.** It receives pre-resolved
  `SecretReference` values with file paths and validated, role-specific data.
- **The controller must not generate NGINX config text or render templates.** It hands complete extended
  resources to the Configurator.
- **The Gateway API layer must not write NGINX configuration.** It produces VirtualServers and
  TransportServers that take the same path as those from the API. What a VirtualServer cannot express
  (the filters of the Gateway API) travels in an annotation, see [Gateway API](#gateway-api).
- **`internal/third_party/ngf/` is never edited by hand.** It is a copy that `hack/sync-ngf.sh` makes and
  a test checks; changes go into `hack/ngf-patches/` or `internal/gateway/`.
- **The data model (`types.go`) must not import `internal/configs` or `internal/k8s`.**
- **Validation must not trigger reloads or update status.** It returns errors; the caller decides.

---

## Data flow: from `kubectl apply` to NGINX

```text
kubectl apply -f virtualserver.yaml
  │
  ▼
Informer sees the event (Add/Update/Delete)
  │  [internal/k8s/handlers.go — createVirtualServerHandlers()]
  ▼
Event handler puts a task on the sync queue
  │  [internal/k8s/controller.go — AddSyncQueue()]
  ▼
Sync loop takes the task and dispatches it
  │  [controller.go — sync() → syncVirtualServer()]
  ▼
Update in-memory state → []ResourceChange
  │  [internal/k8s/configuration.go — AddOrUpdateVirtualServer()]
  │
  ├─► Validation (CRD fields)          [pkg/apis/configuration/validation/]
  ├─► Validation (Ingress annotations) [internal/k8s/validation.go]
  ├─► Host conflicts between Ingresses, VirtualServers and Gateways
  ├─► Affected resources (fan-out on Secret and Policy changes)
  │     [configuration.go — FindResourcesForSecret() / FindResourcesForPolicy()]
  ▼
Resolve secret references by (namespace/name, role)
  │  [controller.go — createVirtualServerEx(), add*SecretRefs()
  │   → secretStore.GetSecret(key, role)]
  ▼
Build the extended resource (VirtualServerEx)
  │  VirtualServer + VirtualServerRoutes + endpoints + Policies + SecretRefs
  ▼
Configurator generates the configuration
  │  [internal/configs/configurator.go]
  │  HTTP:     GenerateVirtualServerConfig()   → version2.VirtualServerConfig
  │  Ingress:  generateNginxCfg()              → version1.IngressNginxConfig
  │            (+ parseIngressNginxAnnotations() for ingress-nginx annotations)
  │  Stream:   generateTransportServerConfig() → version2.TransportServerConfig
  │  Policies: generatePolicies()
  ▼
Render the templates → config text
  ▼
Changed beyond the servers of upstreams?            [internal/configs/dynamic.go]
  ├─ yes: write the file, reload NGINX               [internal/nginx/]
  │       (rollback on a failed nginx -t)
  └─ no (-dynamic-upstreams): send the servers, canaries, balancing rules and
          certificates to the module, no reload
  ▼
Update status and emit Kubernetes events
   [controller.go — updateVirtualServerStatusAndEvents(), k8s/status.go]
```

Status is written after the reload has returned. During startup, before NGINX is ready, status updates
are collected and written after the first successful reload.

| Resource | Sync handler | Extended resource | Config generator |
| --- | --- | --- | --- |
| VirtualServer | `syncVirtualServer()` | `VirtualServerEx` | `GenerateVirtualServerConfig()` |
| Ingress | `syncIngress()` | `IngressEx` / `MergeableIngresses` | `generateNginxCfg()` |
| TransportServer | `syncTransportServer()` | `TransportServerEx` | `GenerateTransportServerConfig()` |
| Policy | `syncPolicy()` | (fans out to VirtualServers and Ingresses) | `generatePolicies()` |
| Secret | `syncSecret()` | (fans out) | (through the affected resources) |
| Gateway API resources | `syncGatewayAPI()` | VirtualServers and TransportServers without an API object | as VirtualServer and TransportServer |
| `tcp-services` / `udp-services`, default backend | `syncIngressNginxStreams()`, `syncIngressNginxDefaultBackend()` | TransportServers, the default server | as TransportServer |

---

## ingress-nginx compatibility

Most of it lives in files of its own, so that merges of upstream releases stay small. The user-facing
behaviour is described in [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md).

| Concern | Where | What it does |
| --- | --- | --- |
| Which Ingresses | `internal/k8s/ingress_class.go` | Serves the IngressClasses whose `spec.controller` is one of `-ingress-class-controllers` (`nginx.wx1.eu/ingress-controller`, and optionally `k8s.io/ingress-nginx` or `nginx.org/ingress-controller` to take over another controller's class), the class `-ingress-class`, Ingresses without a class with `-watch-ingress-without-class` or a default class. |
| The shape of an Ingress | `internal/k8s/ingress_shape.go` — `asIngressNginx()` | Rewrites an Ingress into the shape ingress-nginx serves: rules of one host merged, the first location of a path kept, `spec.defaultBackend` as a catch-all. Used by the configuration and by the admission check alike. |
| Hosts across namespaces | `-ingress-nginx-cross-namespace` | Lets Ingresses of different namespaces share a host, and canaries attach across namespaces. |
| Annotations | `internal/configs/ingress_nginx*.go` — `parseIngressNginxAnnotations()` | Reads `nginx.ingress.kubernetes.io/*` with ingress-nginx's semantics into the same `ConfigParams` as the `nginx.org/*` annotations; one file per area (auth, CORS, rate limits, rewrites, regex, proxy SSL, mirrors, errors, ...). |
| Refused annotations | `internal/configs/ingress_nginx_unsupported.go` | Annotations that restrict access but are not implemented fail closed: the Ingress answers `503`, with a warning, instead of serving what ingress-nginx would refuse. |
| ConfigMap and defaults | `ingress_nginx_configmap.go`, `ingress_nginx_defaults.go` | ingress-nginx's ConfigMap keys, and with `-ingress-nginx-defaults` its defaults wherever neither ConfigMap nor annotations set a value. |
| Canaries | `internal/configs/canary.go` | With `-dynamic-upstreams` a canary Ingress has no servers of its own; its backends become the canaries of the primary Ingress, decided per request in the module. |
| TCP/UDP services, default backend | `internal/k8s/ingress_nginx_services.go`, `internal/configs/ingress_nginx_streams.go` | `tcp-services` and `udp-services` ConfigMaps become TransportServers; `-default-backend-service` answers unknown hosts and `custom-http-errors`. |
| Admission webhook | `internal/k8s/admission.go`, `admission_server.go` | ingress-nginx's validating webhook: renders the Ingress as the controller would and runs `nginx -t` on it, unless snippets are enabled (CVE-2025-1974). |
| Metrics | `internal/metrics/collectors/ingress_nginx*.go` | Exports the module's request series under ingress-nginx's metric names and labels. |

---

## Gateway API

`-enable-gateway-api` turns it on. The design is: **NGF decides what the Gateway API means, the
controller's existing path serves it.**

1. **Graph.** `internal/gateway` fills the input of NGF's graph (`internal/third_party/ngf/`) from
   client-go listers: the GatewayClass `-gateway-class` if it names `-gateway-controller-name`, its
   Gateways, HTTPRoutes, GRPCRoutes, TLSRoutes, TCPRoutes, UDPRoutes, ReferenceGrants, Services and
   Secrets. The graph resolves attachment, conflicts and references exactly as NGF does.
2. **Mapping** (`mapping.go`, `l4.go`). HTTP listeners become VirtualServers: the default Gateway on
   NGINX's default ports gets one per hostname, every other Gateway one per hostname and port. TLS
   passthrough, TCP and UDP listeners become TransportServers. They have no object in the API; they take
   the path of VirtualServers and TransportServers from the API - host conflicts with Ingresses, the
   version2 templates and, with `-dynamic-upstreams`, endpoints without reloads.
3. **Filters** (`extras/`). What a VirtualServer cannot express - header modifiers, path and host
   rewrites, redirects with every status, mirrors - is encoded into an annotation of the VirtualServer and
   applied to its generated config by config generation.
4. **Settings and Policies** (`settings.go`, `policyrefs.go`). ingress-nginx annotations on routes go
   through the parsers of the Ingress path (`configs.ParseGatewaySettings`). NGINX Ingress Controller's
   Policies attach with `wxingress.wizardtales.com/policies`; a Policy of another namespace only with a
   ReferenceGrant, otherwise the route fails closed.
5. **Addresses** (`addresses.go`). Every Gateway but the default one gets a Service in its own namespace,
   owned by the Gateway, without a selector. The leader writes its EndpointSlice with the controller's
   pods and NGINX ports from `-gateway-listener-ports`. One NGINX serves all Gateways; the ports keep
   them apart.
6. **Status** (`status.go`). Only the leader writes the status of the GatewayClass, the Gateways
   (addresses, listeners, whether the config is programmed) and the routes, after the reload that
   applied them.

`features.go` lists what wxingress claims in the GatewayClass status; a feature goes there when its
conformance tests pass.

---

## The data plane module

`ngx_http_wxingress_module` is written in metalang (`dataplane/src/`) and built against the same NGINX
version as the image (`dataplane/build.sh`, see [Building](building.md)). The controller loads it
instead of njs when `-dynamic-upstreams` is set. It writes no configuration; it answers where the
controller's file says `meta_pass;`, on the Unix socket NGINX Plus uses for its API
(`/var/lib/nginx/nginx-plus-api.sock`).

| Source | What it does | API |
| --- | --- | --- |
| `main.c` | **Upstream API**: the subset of NGINX Plus's API that `nginx-plus-go-client` uses, so the controller changes the servers of HTTP and stream upstreams on open-source NGINX the same way it does on Plus. Any error makes the controller reload instead. | `/api/9/http/upstreams/…`, `/api/9/stream/upstreams/…` |
| `main.c` | **Canaries**: ingress-nginx's canary decision per request (weight, header, cookie). | `/api/wxingress/1/canaries/:primary` |
| `main.c` | **Balancers**: ingress-nginx's algorithms, step for step - `round_robin`, `ewma`, `chash`, `chashsubset`, `sticky_balanced`, `sticky_persistent` - with its cookies, so sessions pinned by ingress-nginx stay pinned. | `/api/wxingress/1/balancers/:upstream` |
| `main.c` | **Certificates**: chosen per handshake by SNI (exact name, wildcard, default; optionally bound to a port for Gateways), as ingress-nginx's `certificate.lua`. | `/api/wxingress/1/certificates/:name` |
| `metrics.c` | **Metrics**: ingress-nginx's request counter and histograms, and `ingress_upstream_latency_seconds`, counted in the log phase without allocations. | `/api/wxingress/1/metrics` |
| `jwt.c` | **JWT**: the `jwt` Policy without NGINX Plus. | — |
| `oidc.c` | **OIDC**: the `oidc` Policy without NGINX Plus, with sealed session cookies and revocations. | `/api/wxingress/1/oidc/revocations` |
| `compat.c` | The variables the configuration otherwise asked njs for (escaped request URI, API key hash and status), so NGINX loads no njs. | — |

All state lives in shared memory that the master maps before it forks. Every worker sees the same
servers and rules, and counters and rules survive reloads; a restarted NGINX gets everything again from
the controller. JWT and OIDC are described for users in [JWT and OIDC](../user-guide/jwt-oidc.md).

The module's tests are in `dataplane/tests/` (against a test NGINX) and `dataplane/e2e/` (against a kind
cluster, including the ingress-nginx parity test and the Gateway API conformance suite).

---

## Validation

Validation always happens **before** config generation.

1. **CRD fields** (`pkg/apis/configuration/validation/`): VirtualServer, VirtualServerRoute,
   TransportServer and Policy, with the `field.ErrorList` pattern. Some checks run in the API server
   already, through kubebuilder markers and CEL rules. Policies that need NGINX Plus upstream (`jwt`,
   `oidc`) are accepted with `-dynamic-upstreams`, since the module implements them.
2. **Ingress annotations** (`internal/k8s/validation.go`, and the ingress-nginx parsers, which drop
   invalid values with a warning).
3. **The admission webhook** (`-validating-webhook`) refuses an Ingress before it is stored: it renders
   it as the controller would and, unless snippets are enabled, runs `nginx -t` on the result.

Config generation trusts its input. NGINX performs a final `nginx -t`; if it fails, the
`ConfigRollbackManager` restores the previous configuration.

---

## Secret Store

The secret store (`internal/k8s/secrets/`) resolves Kubernetes Secrets for the
specific purpose declared by each reference site. It sits entirely in the
**controller layer**.

wxingress does not infer Secret purpose from `Secret.type` or from the keys present
in `data`. The reference site supplies a `SecretRole`, and that role determines
the required keys, validation, materialization, and reload behavior.

### Two-phase model

**Phase 1 — Reference-gated caching** (`syncSecret()` and `SecretStore.AddOrUpdateSecret()`):

The secret informer watches secrets in the configured namespaces. During reconciliation,
`syncSecret()` caches referenced and special secrets and evicts unreferenced ones.
At startup `preSyncSecrets()` temporarily primes the store after the informer caches
synchronize, preventing resources from observing existing secrets as missing due to queue
ordering. Initial secrets reconciliation then removes unreferenced entries. If a newly
referenced secret is absent from the local store, the store can resolve it from the
synchronized informer cache.

**Phase 2 — Lazy role resolution** (`SecretStore.GetSecret()`):

When the controller builds an extended resource (`createVirtualServerEx()`,
`createIngressEx()`, `createTransportServerEx()`), it calls `GetSecret()`. For an 
existing secret, the store validates and caches the result by `(namespace/name, role)`. 
Existing invalid secrets are also cached for that role. Missing lookups return an error
reference with the expected path but are not cached, so a later lookup retries the
informer resolver. Valid file-backed roles are materialized under `/etc/nginx/secrets/`
using role-specific filenames. Their expected `Path` is populated even when the secret is
missing or invalid, while `CRLPath` is only set for a valid CA-role secret containing
`ca.crl`. Updates revalidate resolved roles, and deletion removes their cached references
and files.

### Secret roles

Kubernetes `Secret.type` is not used for validation, so any type is accepted. The `Opaque` 
type is recommended, or `kubernetes.io/tls` for TLS secrets. Legacy `nginx.org/*` and 
`nginx.com/*` types remain accepted.

| Role | Required or Recognized Keys | Used for |
| --- | --- | --- |
| `RoleTLS` | Required `tls.crt` and `tls.key` | TLS server certs |
| `RoleCA` | Required `ca.crt`; optional `ca.crl` | CA cert (mTLS / upstream trust) |
| `RoleJWK` | Required `jwk` | JWT validation keys |
| `RoleHtpasswd` | Required `htpasswd` | HTTP Basic auth |
| `RoleOIDC` | Required `client-secret` | OIDC client secret |
| `RoleAPIKey` | Client IDs and credentials | API key auth |
| `RoleLicense` | Required `license.jwt` | NGINX Plus license |
| `RoleWAFBundle` | Required `token`, or `username` and `password`; optional `ca.crt` | Bundle-fetch credentials |

WAF HTTPS Bundle sources use `RoleTLS` for their client certificate secret.
N1C and NIM bundle sources use `RoleWAFBundle`. Their `trustedCertSecret`
references use `RoleCA`.

### Special secrets

Some secrets are configured directly by the controller rather than referenced
by workload resources (default server TLS, wildcard TLS, NGINX Plus license, mgmt
client cert, mgmt trusted CA). The controller assigns roles from the configured
reference. TLS for the default, wildcard and mgmt client cert secrets, License
for the license secret and CA for the mgmt trusted CA secret.

One Kubernetes Secret may satisfy multiple special roles. The controller validates all
matching roles before writing any special representation, writes every required
fixed path, and aggregates reload behavior so the strongest required action is
performed once.

A trusted-CA update regenerates all configuration. License and management
client-auth updates require an NGINX reload. Default and wildcard certificate
updates can avoid a master reload when dynamic SSL reload is enabled.

### Key invariant

**The controller resolves secrets and attaches the resolved references; config
generation consumes `secrets.SecretReference` values only.** The extended
resources (`VirtualServerEx.SecretRefs`, `IngressEx.SecretRefs`, `TransportServerEx.SecretRefs`)
carry `map[secrets.SecretRefKey]*secrets.SecretReference`. `SecretRefKey` contains
both the namespaced secret key and its role. Config generation may read `Path`,
`CRLPath` and role-specific data from `.Secret.Data`, but it must not inspect
`Secret.type`, call `SecretStore.GetSecret()` or access the Kubernetes API
directly.

---

## NGINX reload and rollback

### Normal reload flow

1. The `Configurator` calls `Manager.CreateConfig(name, content)` to write the rendered file.
2. It calls `Manager.Reload()`, which writes a new config version, sends `nginx -s reload`, waits for
   NGINX to confirm that version and records the reload metrics.

`-sync-rate-limit` limits reloads per second with `-dynamic-upstreams`; changes that arrive in the
meantime are applied in one reload.

### Rollback protection (`ConfigRollbackManager`)

With rollback enabled, `CreateConfig()` keeps the old file, writes the new one and runs `nginx -t`. If the
test fails, it restores the old file, tests again and reloads with the known-good configuration; if even
that fails, the bad file is deleted (unless it is `nginx.conf`). A single bad resource cannot take down
NGINX.

### Changes without a reload (`-dynamic-upstreams`)

With the module, NGINX is reloaded only when its configuration changes, as in ingress-nginx.
`internal/configs/dynamic.go` compares the rendered configuration without the servers of upstreams; if
only those changed, they go to the module instead (`internal/k8s/dynamic.go` coordinates the rest):

- the servers of upstreams, for Ingresses, VirtualServers, TransportServers, the TCP and UDP services and
  the Gateway API;
- canaries and their weights, and the balancing rules;
- certificates (with `-ssl-dynamic-reload`);
- files NGINX reads per request: basic auth user files and the JWK sets of `jwt` Policies;
- OIDC session revocations (`-oidc-revocations-configmap`).

With NGINX Plus (`-nginx-plus`, inherited, not built by wxingress), the Plus API takes the upstream
servers instead.

---

## Leader election and status

All replicas serve traffic; one is the leader (`internal/k8s/leader.go`, a Lease). Only the leader
writes status - of Ingresses, VirtualServers, Policies and the Gateway API resources - and the
EndpointSlices of Gateway Services. When it stops, it gives up the Lease (`ReleaseOnCancel`) once its
queue is drained, so the next replica leads within seconds.

---

## Templates

| Pipeline | Resources | Package | Templates |
| --- | --- | --- | --- |
| Version 1 | Ingress | `internal/configs/version1/` | `nginx.ingress.tmpl`, `nginx-plus.ingress.tmpl` |
| Version 2 | VirtualServer, VirtualServerRoute, TransportServer, everything from the Gateway API | `internal/configs/version2/` | `nginx.virtualserver.tmpl`, `nginx.transportserver.tmpl` and their `nginx-plus.*` variants |

- The main templates (`nginx.tmpl`, `nginx-plus.tmpl`) produce `nginx.conf`. `nginx.tmpl` loads the
  wxingress module with `-dynamic-upstreams` and njs otherwise.
- Both pipelines share `generatePolicies()` in `internal/configs/policy.go`.
- The OSS templates are the ones wxingress builds and tests. The Plus templates are inherited; keep
  shared directives in both so upstream merges stay small, but Plus-only directives never belong in the
  OSS templates.

---

## Key source files

| File | What to look at it for |
| --- | --- |
| `pkg/apis/configuration/v1/types.go` | CRD structs, the source of truth for the API |
| `internal/k8s/controller.go` | Sync handlers, extended resources, status |
| `internal/k8s/configuration.go` | In-memory state, host conflicts, change detection |
| `internal/k8s/ingress_class.go`, `ingress_shape.go` | Which Ingresses are served, in ingress-nginx's shape |
| `internal/k8s/gateway_api.go` | Where the Gateway API layer meets the controller |
| `internal/k8s/dynamic.go`, `internal/configs/dynamic.go` | What is applied without a reload |
| `internal/k8s/admission.go` | The admission webhook |
| `internal/k8s/oidc_revocations.go` | OIDC revocations between replicas |
| `internal/k8s/secrets/store.go` | The secret store |
| `internal/configs/configurator.go` | Ties config generation to NGINX |
| `internal/configs/ingress_nginx.go` | Entry point of the ingress-nginx annotations |
| `internal/configs/policy.go` | `generatePolicies()` |
| `internal/gateway/mapping.go`, `l4.go` | Gateway API → VirtualServers and TransportServers |
| `internal/gateway/addresses.go` | Services and ports of Gateways |
| `internal/nginx/manager.go`, `rollback_manager.go` | NGINX process, reload, rollback |
| `dataplane/src/main.c` | The module: upstream API, canaries, balancers, certificates |
| `dataplane/src/jwt.c`, `oidc.c`, `metrics.c` | JWT, OIDC, metrics in the module |
