# Ingresses written for ingress-nginx

wxingress reads the `nginx.ingress.kubernetes.io/*` annotations of ingress-nginx
with the semantics of ingress-nginx, so that an Ingress written for it behaves
the same here. The code is in `internal/configs/ingress_nginx*.go` and
`internal/configs/canary.go`.

General rules, as in ingress-nginx:

- An invalid annotation is ignored, with a warning event on the Ingress; values
  are trimmed line by line first.
- Where an annotation takes nginx variables (`rewrite-target`,
  `x-forwarded-prefix`, `permanent-redirect`, `temporal-redirect`,
  `upstream-vhost`, `app-root`, `proxy-redirect-from`/`-to`, `auth-url`,
  `auth-signin`, `auth-cache-key`, `auth-proxy-set-headers`, `mirror-target`,
  `proxy-ssl-name`, `proxy-cookie-domain`/`-path`, `upstream-hash-by`,
  `fastcgi-params-configmap`), only variables every server knows are taken:
  `$1` to `$9` (the captures of the path's regular expression), the request's `$http_<header>`, `$arg_<name>` and `$cookie_<name>`, and `$args`, `$binary_remote_addr`, `$content_length`, `$content_type`, `$document_root`, `$document_uri`, `$escaped_request_uri`, `$fastcgi_path_info`, `$fastcgi_script_name`, `$host`, `$hostname`, `$https`, `$is_args`, `$proxy_add_x_forwarded_for`, `$proxy_host`, `$proxy_port`, `$query_string`, `$realpath_root`, `$remote_addr`, `$remote_port`, `$remote_user`, `$request_filename`, `$request_id`, `$request_method`, `$request_uri`, `$scheme`, `$server_addr`, `$server_name`, `$server_port`, `$server_protocol` and `$uri`. A value with another variable (`$best_http_host`
  included, which wxingress does not define), a `$` without a name or a brace
  outside `${name}` where the value is unquoted is invalid: an unknown variable
  would make `nginx -t` fail for every Ingress. Where an invalid value denies
  the locations (`auth-url`, `auth-proxy-set-headers`,
  `fastcgi-params-configmap`), it does so here as well.
- A regular expression nginx compiles (a `use-regex` or `rewrite-target` path,
  a `~` of `proxy-redirect-from`, `proxy-cookie-path` or `proxy-cookie-domain`)
  must be one PCRE takes: .NET's balancing groups (`(?<a-b>...)`,
  `(?<-a>...)`), group names starting with a digit, conditions that are no
  group (`(?(text)yes|no)`), `\p{IsBlock}` and a quantifier other than `{n}` in
  a lookbehind are refused; an Ingress with such a path is rejected.
- Where an `nginx.org/*` annotation sets the same, the `nginx.org/*` one wins,
  with a warning.
- Where ingress-nginx refuses to serve a location because its auth cannot be
  set up (a missing auth Secret, an invalid `auth-url`, ...), the locations of
  the Ingress answer 503 instead of serving without the auth.
- An annotation of ingress-nginx that is not listed below is ignored with a
  warning event on the Ingress (`... is not supported by wxingress and is
  ignored`), typos included. Those that restrict who gets an answer make all
  locations of the Ingress answer 503 instead, with a warning event, since
  serving without them would serve what ingress-nginx refuses: `enable-modsecurity` and `enable-owasp-core-rules` (other than `false`),
  `modsecurity-snippet` and `global-rate-limit` (other than 0).
  `enable-global-auth` is only ignored with a warning: wxingress has no
  global auth to turn on or off (the ConfigMap's `global-auth-*` keys are not
  supported, below). That holds for Ingresses; on
  Gateway API routes only the annotations in "The Gateway API" are read, and
  each other one is a warning in the route's status and events. Those that
  restrict access make the rules of the route answer 503 instead, as the
  locations of an Ingress, with a warning in the log (a Policy through
  `wxingress.wizardtales.com/policies` restricts a route).

## Which Ingresses

As ingress-nginx serves the IngressClasses of its `--controller-class`,
wxingress serves those whose `spec.controller` is in `-ingress-class-controllers`
(chart `controller.ingressClass.controllerValue` and
`controller.ingressClass.controllers`; default `nginx.wx1.eu/ingress-controller`).
With `k8s.io/ingress-nginx` in the list it serves ingress-nginx's classes -
beside it during a migration, with `controller.ingressClass.create=false` so
that the chart does not take the class over. The class an Ingress names in
`kubernetes.io/ingress.class` wins over `ingressClassName`, as in
ingress-nginx. Ingresses without a class are served with
`-watch-ingress-without-class` (chart default on) or when a served
IngressClass is the cluster's default. A change of an IngressClass - created,
deleted, another controller - changes what is served without a restart.

Shapes ingress-nginx takes and NGINX Ingress Controller refused: the same
host in several rules of one Ingress (the paths of all of them), and an
Ingress with only `spec.defaultBackend` (a catch-all for every host, with
`-allow-empty-ingress-host`; with `-ingress-nginx-defaults` it serves plain
http as ingress-nginx does, where NGINX Ingress Controller sends it to https
because the default server has the default certificate).

## Several Ingresses of one host

As ingress-nginx does, regular Ingresses that share a host make one server:
the oldest Ingress of the host holds it, and the paths the others of its
namespace have for the host become locations of its server. The annotations of
each Ingress apply to its own paths; the settings of the server (redirects,
HSTS, server snippets) are those of the holder, and its certificate is that of
the holder or, if it has no TLS for the host, of the first other Ingress that
has. A path the server has already stays with the Ingress that has it first,
with a warning on the other. An Ingress whose every host is merged so gets an
event that names the Ingress whose server has its paths; a change to it
changes that server. Masters and minions (`nginx.org/mergeable-ingress-type`)
and VirtualServers keep the controller's rule: the host is theirs, and the
other resources of the host are rejected.

### Hosts across namespaces: `-ingress-nginx-cross-namespace`

ingress-nginx merges the Ingresses of a host whatever their namespace, and a
canary Ingress of any namespace attaches to the Ingresses of the host it
names. In a cluster shared by teams, that lets whoever may create an Ingress
in one namespace add paths to the host of another namespace - a `/login` in
front of someone else's application - or send its traffic, or a share of it,
to a Service of their own with a canary. wxingress therefore keeps a host in
the namespace of the Ingress that holds it unless told otherwise:

- an Ingress of another namespace with that host loses it, with the warning
  `host <host> is taken by an Ingress of another namespace` (and, if it has
  no other host, the event `All hosts are taken by other resources`); the
  warning names neither the holder nor its namespace;
- a canary Ingress attaches only to the Ingresses of its own namespace; one
  that finds its host and path only in another namespace gets the event
  `The canary has no Ingress with the same host, path and path type in its
  namespace`. With `-ingress-nginx-defaults` a canary is the canary only of
  upstreams (namespace, Service, port) of its own namespace, and the upstream
  is told by namespace, Service and port, not by ingress-nginx's name
  `<namespace>-<service>-<port>`, which namespace `a-b` with Service `c` shares
  with namespace `a` with Service `b-c`.

Clusters that rely on ingress-nginx's behaviour - a WAF or an authentication
proxy in a namespace of its own that adds a canary or paths to the hosts of
other namespaces - turn it on with `-ingress-nginx-cross-namespace` (Helm:
`controller.ingressNginxCrossNamespace: true`). It is not part of
`controller.ingressNginxCompat`: turn it on only where everyone who may create
Ingresses may also take over the hosts of every namespace.

### Server aliases

A `server-alias` never takes a host away. nginx serves a name with the server
it reads first, so an alias that competed with a host would decide by the
order of the config files. As ingress-nginx skips an alias that is a host or
an alias already, wxingress leaves out of the server, with a warning on its
Ingress (`server-alias <name> is ignored: ...`), every alias that

- is the host of another resource (Ingress, VirtualServer, TLS-passthrough
  TransportServer), compared without case, or, as a wildcard
  (`*.example.com`), matches one, or falls under another resource's wildcard
  host - nginx prefers the exact name and the longer wildcard, so either would
  take requests from the other;
- collides in the same way with an alias of an older Ingress.

An alias that is a host of the Ingress itself is no conflict and simply not
repeated; an alias is given once, to the first server of the Ingress. When the
other resource goes, the alias comes back with the next sync.

## Prefix paths

A path of type Prefix matches the path and what is below it, as the
Kubernetes API and ingress-nginx have it: `/p` matches `/p` and `/p/...`, not
`/px`. As in ingress-nginx it is rendered as `location /p/` and `location = /p`
(without the second when the Ingress has an Exact path `/p`); `/` and paths
that end in `/` stay as they are, and so do paths that are regular expressions
(`use-regex`, `nginx.org/path-regex`) or are rewritten.

## Reloads

With `-dynamic-upstreams` NGINX is reloaded, as ingress-nginx reloads it, only
when its configuration changes. The configuration does not change with the
servers of an upstream: ingress-nginx compares its configuration without the
endpoints of its backends, and here a config whose file differs only in the
`server` lines of its upstreams - addresses and their parameters, not a server
by name, which NGINX resolves when it loads the config - is not a change. The
file still gets those servers, since a reload starts the upstreams with what
the file has, and the servers go to the wxingress module through its upstream
API. An update of an Ingress or a VirtualServer that changes nothing else -
a label, an annotation that renders the same, new endpoints - reloads nothing:
the module gets the servers, canaries and certificates that changed. The
controller remembers which servers the module has for each upstream, so it
calls the API only for those that changed. A TransportServer is handled the
same way: the upstreams of the `stream` block that its config has get their
servers through the stream part of the same API (`/api/9/stream/upstreams/`),
with the `max_fails`, `max_conns` and `fail_timeout` its upstream gives them,
so new endpoints of a TCP or UDP service reload nothing either. The same
holds for the ConfigMap: a change of it regenerates every config, and reloads
only when one of them, the main config or the dhparam changed.

An EndpointSlice that is added, changed or deleted queues its Service, once
however many of its EndpointSlices change - a deleted one names its Service in
its labels, so its servers go too, also when its Service stays. The Services
whose EndpointSlices changed get their servers together, at once after a quiet
time and then at most ten times a second, and only the upstreams whose servers
changed are sent. Their configs are not regenerated and written for every
change: the file of a config gets its new servers right before the next
reload, so a reload never starts an upstream with servers it no longer has.

Reloads are rate limited as ingress-nginx's syncs are: `-sync-rate-limit`
(Helm: `controller.syncRateLimit`) is the most reloads a second, by default
0.3 as in ingress-nginx. A change after a quiet time is reloaded at once; one
that comes sooner after a reload waits for the end of the interval, and every
change that comes meanwhile is in that one reload. What needs no reload -
servers, canaries, certificates, basic-auth files - does not wait, except a
canary rule while a reload waits, since it may name an upstream that reload
brings. When events come faster than the controller handles them, it handles
them as one batch and reloads at its end when something in it changed the
configuration, whatever the events were.

## Certificates

With `-dynamic-upstreams` the certificates of hosts are not named in the
configuration: as in ingress-nginx, the controller sets them by name in the
wxingress module, in shared memory, and the module picks one for each TLS
handshake by the name the client sent - that name, in lower case and without a
trailing dot, then the wildcard one level above it (`*.example.com` for
`a.example.com`, not for `a.b.example.com`), then `_`, the default certificate.
A TLS Secret that is added, changed or removed, and an Ingress that names
another Secret, change no configuration and cause no reload: the next handshake
has the new certificate. Each worker parses a certificate once and keeps it
until the controller sets another for its name.

Every TLS server names the default server's certificate
(`/etc/nginx/secrets/default`), which is what a name without a certificate of
its own gets - also a host whose Secret is missing or invalid, which ingress-nginx
serves the default certificate as well, where the controller without the module
rejects the handshake. Without `-default-server-tls-secret` that is the fake
certificate of ingress-nginx ("Kubernetes Ingress Controller Fake Certificate",
self-signed for `ingress.local`), made when the controller starts.

Since a TLS Secret then changes no configuration, its events regenerate
nothing, as in ingress-nginx, where a Secret only ends up in the next sync: the
controller writes or removes the Secret's file and gives the module what
changed in one go, at most twice a second - however many Secrets came, changed
or went since the last time (ingress-nginx syncs at most every 3.3 s by
default, `--sync-rate-limit=0.3`). Only a Secret that becomes valid or invalid,
comes or goes has the Ingresses and VirtualServers that name it regenerated,
once for all such Secrets in that go, for the hosts the module is to have and
for their warnings; the configuration stays the same. A Secret that is also
anything else - a special Secret, in a Policy, a CA, a JWK, for basic auth, of
a TransportServer or a minion - is handled as before. On kind, rotating all
1000 Secrets of an Ingress with 1000 hosts was served 0.1 s after the API
server had them (more than a minute before), and deleting the namespace with
them took 16 s (5 min before).

The Secrets are still written to `/etc/nginx/secrets`, and what the module is
given is what is in those files. The module keeps its certificates over reloads
and starts without any, so the controller gives it all of them after the first
reload when NGINX starts, as ingress-nginx sends all of them after it starts.

The module's API, on the socket of the upstream API:

| Request | |
|---|---|
| `GET /api/wxingress/1/certificates` | every name, with the generation of its certificate |
| `GET /api/wxingress/1/certificates/:name` | one name, its certificate without the key |
| `PUT /api/wxingress/1/certificates/:name` | `{"certificate": PEM, "key": PEM}`; refused with 400 when they do not belong together |
| `DELETE /api/wxingress/1/certificates/:name` | |

A name is a host, `*.` and a host, or `_`. The module holds up to 32768 names
and 128 MB of PEM.

## Annotations

| Annotations | Support |
|---|---|
| `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` | Whole seconds, rendered with `s`. |
| `proxy-body-size`, `client-body-buffer-size` | `client_max_body_size` (k, m, g) and `client_body_buffer_size` (k, m). |
| `proxy-buffering`, `proxy-request-buffering` | `on`/`off`. |
| `proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size` | Rendered as ingress-nginx does: `proxy_buffer_size S` and `proxy_buffers N S`, so either sets both (N defaults to 4, S to 4k or the ConfigMap's). |
| `proxy-max-temp-file-size`, `proxy-http-version` | As in ingress-nginx; `proxy-http-version` takes `1.0` or `1.1`. |
| `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries` | With ingress-nginx's validator; `non_idempotent` is moved to the end. |
| `rewrite-target`, `use-regex`, `x-forwarded-prefix` | A `rewrite-target` or `use-regex` makes every path of the server (of every Ingress on the host) a case-insensitive regular expression `~* "^path"`, ordered longest first as ingress-nginx orders them; `rewrite-target` with `$1`-style captures. `x-forwarded-prefix` is sent only from the locations `rewrite-target` rewrites. `nginx.org/rewrite-target` and `nginx.org/rewrites` win over `rewrite-target`, `nginx.org/path-regex` over `use-regex`. |
| `permanent-redirect`, `permanent-redirect-code`, `temporal-redirect`, `temporal-redirect-code` | Every location of the Ingress answers `return <code> "<url>"`, as in ingress-nginx: `temporal-redirect` wins with 302, `permanent-redirect` has 301, a code outside 300-308 is the default with a warning. The URL must be http or https; a `$` may only start an nginx variable (`https://example.org$request_uri`). |
| `service-upstream` | Proxies to the ClusterIP of the Service; a headless Service to its endpoints. |
| `ssl-redirect`, `force-ssl-redirect`, `preserve-trailing-slash` | Plain http to https as ingress-nginx sends it: `https://<Host header without its port><URI>` with ingress-nginx's code (308 with `-ingress-nginx-defaults`) and the trailing slash of the URI dropped unless `preserve-trailing-slash`. `ssl-redirect` (default true) where the host has TLS, `force-ssl-redirect` also without. ACME challenges and the health URI are not redirected. As in ingress-nginx the redirect is sent by each location, in its rewrite phase: after nginx answered 413 to a body over the location's `proxy-body-size`, after the location's own `return`s (`permanent-redirect`, a CORS preflight, a location that answers 503), before rewrite-target's rewrite. Whether a host redirects is decided per server (the first Ingress of a host has its say), where ingress-nginx decides per location. |
| `enable-cors`, `cors-allow-origin`, `cors-allow-methods`, `cors-allow-headers`, `cors-allow-credentials`, `cors-expose-headers`, `cors-max-age` | ingress-nginx's CORS with its defaults and validators: the headers for an allowed origin (`*` without `cors-allow-origin`; `https://*.example.com` takes one label), a preflight (OPTIONS) of an allowed origin answered 204 before auth, other origins without headers and to the backend. `add_header ... always` with `add_header_inherit merge` instead of `more_set_headers`, so the server's headers stay. |
| `auth-tls-secret`, `auth-tls-verify-client`, `auth-tls-verify-depth`, `auth-tls-error-page`, `auth-tls-pass-certificate-to-upstream`, `auth-tls-match-cn` | Client certificates on the server of the host, from the `ca.crt` (and `ca.crl`) of the Secret, kept in step with it: `ssl_verify_client` (default on), `ssl_verify_depth` (1), `error_page 495 496`, the `ssl-client-verify`, `ssl-client-subject-dn`, `ssl-client-issuer-dn` headers (and `ssl-client-cert`) to the backend, `auth-tls-match-cn` as 403 `client certificate unauthorized`. A Secret of another namespace or a missing one make the locations answer 503. A host without TLS has https with the default certificate (`-ingress-nginx-defaults`, as in ingress-nginx), where the client certificate is checked; its plain http answers 503 (ingress-nginx serves it without the check). Without the defaults, a host without TLS makes the locations answer 503. An `nginx.org` IngressMTLS Policy wins. |
| `from-to-www-redirect` | A server for `www.<host>` (or `<host>` without `www.` for a `www.` host) that answers everything with ingress-nginx's redirect `<scheme>://<host>:<port><URI without trailing slash>`, with TLS where the Ingress's TLS covers that name - unless a resource has that name; when one takes it or lets it go, the Ingress is configured again. |
| `custom-http-errors`, `default-backend` | `default-backend` names a Service of the namespace (its first port), kept in step with its endpoints. `custom-http-errors` (300-599) sends those answers of the backends to it as ingress-nginx does: `/`, with `X-Code`, `X-Format`, `X-Original-URI`, `X-Namespace`, `X-Ingress-Name`, `X-Service-Name`, `X-Request-ID`. And a 502 or 503 goes to it with the request as it was - nginx's own when the Service has no endpoints (ingress-nginx's case), but also one a backend answers itself. Without `default-backend` the codes go to the controller's `-default-backend-service` (below), as in ingress-nginx, without the 502/503 fallback; without either `custom-http-errors` is ignored with a warning. |
| `mirror-target`, `mirror-host`, `mirror-request-body` | `mirror` to an internal location that proxies to the target (an http(s) URL, nginx variables allowed - `$request_uri` is the mirrored request's), resolved per request with the pod's name servers, Host the target's or `mirror-host`; the mirror's answer is not waited for. |
| `proxy-ssl-secret`, `proxy-ssl-verify`, `proxy-ssl-verify-depth`, `proxy-ssl-ciphers`, `proxy-ssl-protocols`, `proxy-ssl-name`, `proxy-ssl-server-name` | TLS to the backends (with `backend-protocol: HTTPS`) with ingress-nginx's defaults - verify off, depth 1, `TLSv1.2`, `DEFAULT` ciphers, server name off: the client certificate from `tls.crt`/`tls.key` and the CA from `ca.crt` of a Secret of the namespace (503 if it has neither). An `nginx.org` egressMTLS Policy wins. |
| `ssl-passthrough` | With `-enable-tls-passthrough` (chart `controller.enableTLSPassthrough`, ingress-nginx's `--enable-ssl-passthrough`): per host a TransportServer of the TLS passthrough listener, made by the controller, that sends the TLS connections of that name untouched to the Service of path `/` (or the first path), endpoints without a reload. Without the flag ignored with a warning, as ingress-nginx ignores it without its flag. |
| `enable-access-log` | `false`: `access_log off` in the locations of the Ingress. |
| `enable-rewrite-log` | `true`: `rewrite_log on` in the locations of the Ingress (the rewrites are logged in the error log at `notice`). |
| `proxy-cookie-domain`, `proxy-cookie-path` | `proxy_cookie_domain` and `proxy_cookie_path` in the locations of the Ingress, with the value `<from> <to>` or `off`; another value is ignored with a warning. |
| `relative-redirects` | `true`: `absolute_redirect off` in the locations of the Ingress, so nginx's own redirects are relative; `false` leaves nginx's default. |
| `use-port-in-redirects` | `port_in_redirect on` (`true`) or `off` (`false`) in the locations of the Ingress. |
| `satisfy` | `satisfy any` or `satisfy all` in the locations of the Ingress: with `any`, a request that passes one of the source ranges, basic auth and external auth is served. Another value is ignored with a warning. |
| `disable-proxy-intercept-errors` | `true`: the codes of `custom-http-errors` are not intercepted, the backends' answers go to the client as they are. A `default-backend` still gets the 502 and 503 (above). |
| `server-alias` | More names of the server, comma- or space-separated host names. A wildcard stands for one label and must leave at least two (`*.example.com`; `*.com`, `*` and `example.*` are ignored with a warning). |
| `upstream-vhost`, `app-root`, `ssl-ciphers`, `ssl-prefer-server-ciphers`, `proxy-redirect-from`, `proxy-redirect-to` | As `nginx.org/upstream-vhost`, `nginx.org/app-root`, `nginx.org/ssl-ciphers`, `nginx.org/ssl-prefer-server-ciphers`, `nginx.org/proxy-redirect-from` and `-to` (which win); a value with characters that would break out of the directive is ignored with a warning, as is one the controller would refuse in the `nginx.org/*` annotation (`#`, a backtick, a regular expression PCRE does not compile in `proxy-redirect-from`). `proxy-redirect-from` other than `off` or `default` without `proxy-redirect-to`, and `proxy-redirect-to` without `proxy-redirect-from`, are ignored with a warning. |
| `backend-protocol` | `HTTPS`, `GRPC`, `GRPCS` for every Service of the Ingress (as `nginx.org/ssl-services`, `nginx.org/grpc-services`); `HTTP`, `AUTO_HTTP` as they are; `FCGI` below; any other answers 503. |
| `backend-protocol: FCGI`, `fastcgi-index`, `fastcgi-params-configmap` | The locations pass to their upstreams with FastCGI as in ingress-nginx: nginx's `fastcgi_params`, `fastcgi_index`, and a `fastcgi_param` for each key of a ConfigMap of the Ingress's namespace (watched; keys and values validated as ingress-nginx does). A missing ConfigMap, one of another namespace or an invalid parameter: 503. |
| `allowlist-source-range` (`whitelist-source-range`), `denylist-source-range` | Comma-separated addresses and CIDRs, IPv4 and IPv6, in every location of the Ingress as ingress-nginx renders them: `deny` of each denied range first, then `allow` of each allowed one and `deny all` (403). `allowlist-source-range` wins over `whitelist-source-range`. An invalid range makes the locations answer 503, as ingress-nginx denies them. |
| `limit-rps`, `limit-rpm`, `limit-connections`, `limit-burst-multiplier`, `limit-allowlist` (`limit-whitelist`), `limit-rate`, `limit-rate-after` | ingress-nginx's zones (5m, keyed by `$binary_remote_addr`), allowlist (`limit-allowlist` wins over `limit-whitelist`), bursts (limit × multiplier, default 5, `nodelay`) and its 503; `limit-rate` and `limit-rate-after` in kilobytes (`limit_rate <n>k`). An invalid limit or allowlist drops the whole rate limit, as there. `nginx.org/limit-req-rate` wins over `limit-rps` and `limit-rpm`. |
| `auth-type`, `auth-secret`, `auth-secret-type`, `auth-realm` | Basic auth from the Opaque Secret (`auth-file` with key `auth`, or `auth-map`), kept in step with the Secret; the Authorization header is not passed on. `auth-type: digest` answers 503: open-source nginx has no digest auth. |
| `auth-url`, `auth-method`, `auth-signin`, `auth-signin-redirect-param`, `auth-response-headers`, `auth-request-redirect`, `auth-snippet`, `auth-cache-key`, `auth-cache-duration`, `auth-always-set-cookie` | External auth with `auth_request`, its internal location and headers as in ingress-nginx, and the 302 to the sign-in URL with `rd=` on 401. `auth-snippet` needs `-enable-snippets`. |
| `auth-proxy-set-headers` | The headers of a ConfigMap of the Ingress's namespace (`name` or `<namespace>/name`) to the auth service, nginx variables in the values as in ingress-nginx; watched, a change configures the Ingress again. A missing ConfigMap, one of another namespace (wxingress takes no resources of other namespaces), an invalid header name or a value ingress-nginx would not take as a header value (a line break or other control character) or with a variable not every server knows: 503. |
| `auth-keepalive`, `auth-keepalive-requests`, `auth-keepalive-timeout`, `auth-keepalive-share-vars` | The auth service as an upstream with `keepalive` (default 1000 requests, 60 s), its name resolved while nginx runs (`server ... resolve`), so a missing auth service fails no reload. As in ingress-nginx not with HTTP/2 on (the ingress-nginx default; ignored with a warning) and not when the host of `auth-url` has a variable. nginx keeps the connection of an auth answer without a body (oauth2-proxy's 202, a `Content-Length: 0`) and closes one with a body, which `auth_request` does not read. `auth-keepalive-share-vars` is what nginx's subrequests do anyway (they share the variables of the request). |
| `connection-proxy-header` | `close` or `keep-alive`: the Connection header to the backend instead of the one the location sends (`$connection_upgrade`, or none for HTTP/2) - exactly one. |
| `custom-headers` | The headers of a ConfigMap of the Ingress's namespace on every answer of its locations, also nginx's own, the backend's of the same name replaced (`proxy_hide_header` and `add_header ... always`, with `add_header_inherit merge` so the server's stay - where ingress-nginx has `more_set_headers`). As in ingress-nginx each header must be in the ConfigMap key `global-allowed-response-headers` (comma-separated), and a `$` of a value is a `$`. Not allowed, missing, of another namespace, an invalid name or value: 503. The ConfigMap is watched (an informer for just it, while an Ingress names it); a change reloads. |
| `enable-opentelemetry`, `opentelemetry-trust-incoming-span`, `opentelemetry-operation-name` | Tracing of the locations with nginx's otel module: `otel_trace on` or `off`, `otel_trace_context propagate` (trusting the span of the request, the default) or `inject`, `otel_span_name`. The traces go to the collector of the ConfigMap's `otel-exporter-endpoint` (`otlp-collector-host`/`-port`) and nowhere else; without one the annotations are ignored with a warning. |
| `stream-snippet` | With `-enable-snippets`: a file of the stream block per Ingress (`stream-conf.d/ingress-snippets_<namespace>-<name>.conf`), gone with the Ingress. |
| `server-snippet`, `configuration-snippet` | The server and location snippets, as `nginx.org/server-snippets` and `nginx.org/location-snippets`, which win over them. They need `-enable-snippets`; without it they are ignored with a warning. |
| `affinity: cookie`, `affinity-mode`, `affinity-canary-behavior`, `session-cookie-name` (default `INGRESSCOOKIE`), `session-cookie-path`, `session-cookie-domain`, `session-cookie-expires`, `session-cookie-max-age`, `session-cookie-secure`, `session-cookie-samesite`, `session-cookie-conditional-samesite-none`, `session-cookie-change-on-failure` | With `-dynamic-upstreams`, ingress-nginx's sticky balancers in the wxingress module (see below): `affinity-mode` balanced or persistent, `session-cookie-change-on-failure`, and its cookie, `<key>\|<backend key>` with Expires, Max-Age, Domain, Path, Secure, HttpOnly and SameSite as ingress-nginx writes it, so its sessions stay pinned. Without it nginx's `sticky cookie`, HttpOnly, with the path of the Ingress, and `session-cookie-change-on-failure` always true. An `nginx.org/sticky-cookie-services` cookie of the same Service wins. |
| `upstream-hash-by`, `upstream-hash-by-subset`, `upstream-hash-by-subset-size`, `load-balance` | `load-balance` is `round_robin` or `ewma`, the subset size 3 by default; with `affinity` both are ignored, and `nginx.org/lb-method` wins over them. With `-dynamic-upstreams`, ingress-nginx's chash, chashsubset, round_robin and ewma in the wxingress module (see below). Without it `hash ... consistent`; `load-balance: ewma` becomes `least_time last_byte`, `round_robin` nginx's round robin, and `upstream-hash-by-subset` hashes to one endpoint, with a warning. |
| `canary`, `canary-weight`, `canary-weight-total`, `canary-by-header`, `canary-by-header-value`, `canary-by-header-pattern`, `canary-by-cookie` | Need `-dynamic-upstreams` (the wxingress data plane); without it, or with an invalid canary annotation, the canary Ingress is ignored with an event. `canary-weight-total` defaults to 100. A canary Ingress makes no servers of its own. With `-ingress-nginx-defaults`, as ingress-nginx's alternative backend: a path of the canary finds the location with its host, path and path type (in the canary's namespace, in any namespace with `-ingress-nginx-cross-namespace`), and the canary's backend becomes the canary of that location's upstream - ingress-nginx's backend of namespace, Service and port - so a request with the canary's header or cookie, or its weight, goes to the canary on every host and path of every Ingress whose backend is that Service's port; an upstream has the first such canary, oldest first, as ingress-nginx uses only its first alternative backend. Without `-ingress-nginx-defaults` the canary is that of the location's Ingress alone. Decided per request; a changed rule (weight, header, cookie) reaches the module without a reload, a canary that attaches to an upstream or leaves one reloads the configs of its Ingresses (ingress-nginx's needs none: its locations name the upstream at request time, wxingress's have the canary upstream in their config). |

## TCP and UDP services, the default backend

`-tcp-services-configmap` and `-udp-services-configmap` (chart
`controller.tcpServices` and `controller.udpServices`, which the chart puts
into a ConfigMap, opens in the pods - with `controller.hostPort.enable` on the
nodes too - and adds to its Service; or `controller.tcpServicesConfigMap` for
an existing one) take ingress-nginx's format, `"<port>":
"<namespace>/<service>:<port>[:PROXY][:PROXY]"`: the port of the Service by
number or name, the first `PROXY` taking the PROXY protocol of the client
(`listen ... proxy_protocol`), the second sending it to the Service
(`proxy_protocol on`; `<port>::PROXY` for that alone), not for UDP. Each entry
is a stream server with ingress-nginx's settings - `proxy_timeout 600s`, the
next upstream on, 3 tries in 600 s, one response a UDP datagram - made by the
controller (no TransportServer object), whose endpoints change without a
reload with `-dynamic-upstreams`; the ConfigMap is watched, an entry that is
added or taken out reloads. An entry the controller cannot take - not a port,
a Service or port that does not exist - is a Warning event on the ConfigMap.

`-default-backend-service=<namespace>/<name>` (chart
`controller.defaultBackendService`) is ingress-nginx's
`--default-backend-service`: its first port answers the requests of an
unknown host (in place of the default 404; a `default-server-return` of the
ConfigMap wins), the paths of a host that no Ingress of it has (a server
without `/` gets a `location /` to it), and the `custom-http-errors` of
Ingresses without a `default-backend` annotation. Its endpoints change without
a reload. An Ingress with only `spec.defaultBackend` (a catch-all) takes the
unknown hosts instead, as in ingress-nginx. A Service without endpoints is no
default backend for nginx's own 503s, as in ingress-nginx.

## Metrics

With `-enable-prometheus-metrics` (chart `prometheus.create`, on the port of
`prometheus.port` - 10254 is ingress-nginx's) the controller exports
ingress-nginx's metrics beside its own, under ingress-nginx's names, labels
and buckets, so that its dashboards (Grafana 9614) and alerts work as they
are. Their constant labels are `controller_namespace`, `controller_pod` and
`controller_class` - the IngressClass controller served in ingress-nginx's
place, `k8s.io/ingress-nginx` where that is one of
`-ingress-class-controllers`.

| metric | as ingress-nginx |
|---|---|
| `nginx_ingress_controller_requests` | a counter per `status`, `method`, `path` (the Ingress's), `namespace`, `ingress`, `service`, `canary` (the upstream a canary sent the request to, else empty) and `host` |
| `..._request_duration_seconds`, `_response_duration_seconds`, `_connect_duration_seconds`, `_header_duration_seconds` | histograms of `$request_time` and of the upstream's response, connect and header times (only for a request that tried one upstream, as ingress-nginx's Lua cannot read a list), `prometheus.DefBuckets` |
| `..._ingress_upstream_latency_seconds` | the summary ingress-nginx exported up to 1.11 (removed in 1.12, so 1.12's and 1.15's dashboards do not use it): `$upstream_connect_time` of a request that tried one upstream, per `ingress`, `namespace`, `service` and `canary`, with the quantiles 0.5, 0.9 and 0.99 of the last 8 to 10 minutes, as client_golang's summary with ingress-nginx's objectives and its default age buckets. The module counts the times in bins - a millisecond each up to 63 ms (nginx's times have milliseconds), then 16 to an octave up to 131 s - and the controller takes the quantiles from the difference of two readings some ten minutes apart, a reading kept every two minutes of the scrapes: exact to the millisecond up to 63 ms, within a sixteenth above, where ingress-nginx's is a sample within the objectives' rank error |
| `..._request_size`, `_response_size`, `_bytes_sent` | histograms of `$request_length` and `$bytes_sent`, `LinearBuckets(10, 10, 10)` and `ExponentialBuckets(10, 10, 7)` |
| `..._config_hash`, `_config_last_reload_successful`, `_config_last_reload_successful_timestamp_seconds`, `_success`, `_errors` | the reloads; the hash is a number that changes with every configuration loaded, not a hash of the file |
| `..._ssl_expire_time_seconds`, `_ssl_certificate_info` | per host of an Ingress or VirtualServer with TLS, labels `namespace` (of the Secret), `class`, `host`, `secret_name`, `identifier` and the issuer, serial and key algorithm; a certificate file is parsed again when it changed |
| `..._nginx_process_*` | the nginx processes, master and workers: connections and requests from `stub_status`, CPU, memory, I/O, their number and the oldest start time from `/proc` |

The request metrics need `-dynamic-upstreams`: the wxingress module counts
them in nginx's log phase (`dataplane/src/metrics.c`), for the locations the
controller's config marks with `$wxingress_metrics` - the labels of the
location in one constant string, so the module reads one variable, builds
nothing and allocates nothing per request. The counts are in shared memory
that outlives reloads, kept per worker and summed when the controller reads
them (`GET /api/wxingress/1/metrics` on the module's socket) at each scrape.
The table has 8192 series; a request beyond them is counted in the series
without labels. `ingress_upstream_latency_seconds` has a table of its own of
2048 series (namespace, Ingress, Service, canary), found through the same
per-worker cache as the location's series, and costs three atomic adds a
request that connected to an upstream. Measured on a trivial proxy (one worker and four, `perf
stat`, instructions per request): +2.0 % (about 630 of 31 200), of which the
module's step and the extra `set` are some 300 by the profile and the rest
is spread over nginx; on a request with TLS and real headers it is less.

With the admission webhook (below), `..._check_success` and
`..._check_errors` count its checks per `namespace` and `ingress`, as
ingress-nginx's do.

Not exported: `orphan_ingress`, `leader_election_status`, `build_info`,
the admission webhook's `admission_*` gauges of the last check's size and
durations, and the request metrics of VirtualServers and Gateway API
routes, which have none in ingress-nginx.

## The admission webhook

With `-validating-webhook=:8443` (chart `controller.admissionWebhooks`,
ingress-nginx's values: `enabled`, `failurePolicy` - `Fail` by default, as
ingress-nginx's - `timeoutSeconds`, `port`, `namespaceSelector`,
`objectSelector`, `service`, the self-signed certificate of the
`patch` jobs with kube-webhook-certgen or `certManager`) the controller
serves a ValidatingWebhook for the Ingresses created and updated
(`/networking/v1/ingresses`, AdmissionReview v1). As ingress-nginx's
`CheckIngress` it lets through an Ingress of another class or one being
deleted, and refuses:

- a snippet annotation (`*-snippet`, `*-snippets`) while snippets are
  disabled (`-enable-snippets`, ingress-nginx's `allow-snippet-annotations`),
  with ingress-nginx's message;
- an Ingress the controller's validation (`internal/k8s/validation.go`)
  rejects - which without the webhook is not served, with a Warning event;
- a path of a host that another Ingress of the class has, whatever their
  path types, unless one of the two has the `canary` annotation, with
  ingress-nginx's message (`host "<host>" and path "<path>" is already
  defined in ingress <namespace>/<name>`) when that Ingress is of the same
  namespace; for one of another namespace the message is `... is already
  defined in another Ingress`, which tells whoever creates the Ingress
  neither the namespace nor the name of someone else's;
- an Ingress whose configuration does not render or does not pass
  `nginx -t`: the configuration is built aside as it would be with the
  Ingress - its own config, those of the Ingresses of its hosts it would be
  merged into, those of the upstreams it would be the canary of - and
  tested with the files nginx runs with but those replaced
  (`-validating-webhook-nginx-test`, on by default). ingress-nginx stopped
  this test for CVE-2025-1974: whoever can reach the webhook can have its
  configuration parsed, and a snippet runs in `nginx -t`. wxingress tests
  only while snippets are disabled, when no annotation can bring a
  directive of its own.

The warnings of the Ingress's config (an annotation that is ignored, a host
another resource has) go back with the answer, which kubectl prints. A check
waits for the controller's sync to finish what it is doing, and the sync for
the check while it builds and renders the configs: they read the same state.
`nginx -t` runs afterwards, without holding the sync up, on a copy of the
configuration taken while the check held it, one check at a time, and is
stopped after 10 seconds (the API server's default timeout of a webhook). A
Secret the check needs for `nginx -t` that no resource uses yet is written for
the test only and removed when the check is done. The webhook takes at most 4
reviews at once - more are answered `503`, a failed call, so the webhook's
`failurePolicy` decides - and none bigger than 3.5 MiB (`413`). The
certificate is read again when its files change (a renewed Secret).

The webhook does not authenticate its callers: API servers present no client
certificate by default, and whoever reaches the port can have Ingresses
checked (built, rendered and, with `-validating-webhook-nginx-test`, parsed by
`nginx -t`). Restrict who reaches it with a NetworkPolicy that admits only the
API server to the webhook port. [the RKE2 runbook](../migration/rke2.md), step 5, puts
it in the place of rke2-ingress-nginx's; `dataplane/e2e/admission-webhook.sh`
checks what it admits, refuses and counts.

## The Gateway API: the same annotations on routes

With `-enable-gateway-api`, an HTTPRoute or GRPCRoute takes the ingress-nginx
annotations of the backends' upstreams - `proxy-*-timeout`,
`proxy-body-size`, `client-body-buffer-size`, `proxy-buffering`,
`proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size`,
`proxy-next-upstream*`, `proxy-http-version`, `service-upstream`,
`load-balance`, `upstream-hash-by*`, `affinity`, `affinity-mode`,
`session-cookie-*`, `limit-rps`, `limit-rpm`, `limit-connections`,
`limit-burst-multiplier`, `limit-allowlist`, `limit-rate`, `limit-rate-after`,
`canary-by-header`, `-header-value`, `-header-pattern`, `canary-by-cookie`,
`backend-protocol` (`HTTPS` and `GRPCS` turn TLS to the backends on; an
`egressMTLS` Policy adds the client certificate and verification), and
with `-enable-snippets` `server-snippet`, `configuration-snippet`,
`nginx.org/server-snippets` and `nginx.org/location-snippets` - read by the
same code as on an Ingress; with `-dynamic-upstreams` the balancer and
affinity are the module's, as below. The canary annotations apply to a rule
with two backends: the second is the canary, the weights are the backends'
(the second may have weight 0, for a canary only the header or cookie
reaches).
Rate limits, basic and external auth, access control and the other
policies are NGINX Ingress Controller's Policies, attached with
`wxingress.wizardtales.com/policies: rate, auth` on the route or the
Gateway. A rule with two weighted backends is a canary of the module: its
weights change without a reload. Redirects, rewrites, header changes and CORS
are the Gateway API's own filters.

`limit-whitelist` is read as `limit-allowlist`. `proxy-request-buffering`,
`proxy-max-temp-file-size` and `affinity-canary-behavior` are accepted on a
route but not applied. The annotations that restrict who gets an answer -
`auth-*`, the source-range annotations, `satisfy`, ModSecurity and
`global-rate-limit` - are not taken on routes: rather than serve a route
without the restriction, its rules answer 503, with a warning. Use the
equivalent Policy instead.

## Balancing without a reload: `-dynamic-upstreams`

With `-dynamic-upstreams` every upstream of an Ingress says `meta_balancer;`
after its method, and the wxingress module chooses the server of each request
and of each retry by a rule the controller sets through its API
(`/api/wxingress/1/balancers/<upstream>`), the way ingress-nginx's Lua balancer
did. The configuration is the same whatever `load-balance`, `upstream-hash-by`
and the affinity annotations say, so changing them reloads nothing; an
upstream without a rule keeps the method of its block (`lb-method`).

The algorithm is the one ingress-nginx's `get_implementation` picks - the
affinity, else `upstream-hash-by` (with `upstream-hash-by-subset`, chashsubset),
else `load-balance` - and each is the Lua one's, step for step:

| algorithm | as in ingress-nginx |
|---|---|
| `round_robin` | each worker in turn from a random start; a server the request already tried is passed over |
| `ewma` | two random servers of those the request has not tried, the lower Peak-EWMA score (connect + response time, 10 s decay). The averages are per address in shared memory: every worker uses them, and they survive reloads. A server added starts at the average of the others, one removed loses its average. |
| `chash` | resty.chash's ring (160 points a server, CRC32), keyed by `upstream-hash-by` with its nginx variables expanded; a key keeps its server, and a new server takes about 1/n of the keys |
| `chashsubset` | the ring over sets of `upstream-hash-by-subset-size` servers (3), a random server of the key's set |
| `sticky_balanced` | the cookie's key on the ring of servers; a new session gets a random key |
| `sticky_persistent` | the cookie's key is md5(backend + address); a new session goes to a random server, and stays there when the backend scales |

Sessions pinned by ingress-nginx stay pinned: the backend in the cookie is
md5(md5(name) + name) of ingress-nginx's backend name,
`<namespace>-<service>-<port>`, and the ring and the persistent keys are
computed as ingress-nginx computes them. A canary's sessions stay on the side
whose backend their cookie names, before header, cookie and weight.

Where it differs: servers are sorted by address before they are split into
subsets, so every replica makes the same subsets; ewma averages a try in only
when it did not fail, where ingress-nginx averages in the last try even when
it failed (a refused connection takes no time and would look fast); a sticky
balancer with no server left to try leaves the try to the block's method.

## Defaults: `-ingress-nginx-defaults`

Without an annotation or ConfigMap key, wxingress uses the defaults of the
controller it is forked from, which differ from those of ingress-nginx. The
flag `-ingress-nginx-defaults` (Helm: `controller.ingressNginxDefaults: true`)
switches to the defaults of ingress-nginx (its `config.go` and `nginx.tmpl`)
for every setting the rendering of wxingress covers. Keys of the ConfigMap and
annotations still win over them. The values that are not specific to Ingresses
(timeouts, buffers, HSTS, TLS, worker settings) apply to VirtualServers too.

The ConfigMap keys are those of wxingress (`proxy-connect-timeout`,
`proxy-buffering`, `keepalive`, `hsts-*`, ...); with the switch the `hsts-*`
keys apply without `hsts`, `hsts-preload` is read, and the ConfigMap takes
ingress-nginx's keys as well (`internal/configs/ingress_nginx_configmap.go`): a
key of the same name as it is, a renamed one under the name of wxingress
(`proxy-body-size` as `client-max-body-size`, `keep-alive`,
`keep-alive-requests`, `upstream-keepalive-connections`,
`max-worker-connections`, `max-worker-open-files`, `use-proxy-protocol`,
`enable-real-ip` with `forwarded-for-header` and `proxy-real-ip-cidr`,
`log-format-upstream`, `log-format-stream`, `log-format-escape-json`, the
`*-snippet` keys, `otlp-collector-host`/`-port`, `use-http2`, `hide-headers`,
`server-name-hash-*`) - a key of wxingress itself wins - and
`whitelist-source-range`, `allowlist-source-range`, `denylist-source-range`
and `block-cidrs` as allow/deny in every server of the resources (an Ingress
with source ranges of its own has those, as in ingress-nginx; one with
`nginx.org/server-snippets` replaces them). Keys wxingress needs nothing for
(`strict-validate-path-type`, `allow-snippet-annotations`, ...) are taken
silently. So are `compute-full-forwarded-for`, `generate-request-id` and
`ssl-session-tickets`, which are not read at all: a value other than the
default is dropped without a warning - `compute-full-forwarded-for: "true"`
still sends `X-Forwarded-For $remote_addr`, `generate-request-id: "false"`
still sends `X-Request-ID`, `ssl-session-tickets: "false"` leaves nginx's
tickets on. The wxingress keys `limit-req-status-code`, `max-conns` and
`slow-start` are taken without a warning too but are never parsed, so they
have no effect. Every other key is a Warning event on the ConfigMap, and those that
restrict access (`global-auth-*`, `enable-modsecurity`, `block-user-agents`,
...) also an error in the log that the Ingresses are served without them.
`hack/ingress-nginx-values` translates the `controller.config` of the chart the
same way and lists what it cannot take. A `proxy-buffer-size` without `proxy-buffers` sizes four buffers of that
size, as ingress-nginx does.

### Taken from ingress-nginx with the switch

The first column names the setting by its ingress-nginx key. Not every one of
those keys is read from the ConfigMap: `proxy-buffers-number`,
`proxy-request-buffering`, `proxy-next-upstream`, `proxy-next-upstream-timeout`,
`proxy-next-upstream-tries`, `proxy-redirect-from` and, below,
`proxy-http-version`, `proxy-cookie-domain`, `proxy-cookie-path` and
`limit-conn-status-code` are refused as ConfigMap keys (a Warning event, the
key is ignored); for them only the default of the table and the annotation of
the same name per Ingress apply (for the buffers the wxingress key
`proxy-buffers`, `"4 4k"`, sets the same). `limit-req-status-code`,
`generate-request-id` and `compute-full-forwarded-for` are taken without a
warning but have no effect: only the default of the table applies (see above).

| Setting (ingress-nginx key) | ingress-nginx | without the switch | with the switch |
|---|---|---|---|
| `proxy-connect-timeout` | 5s | 60s | **5s** |
| `client-body-buffer-size` | 8k | not set (nginx: two pages, 8k on x86-64) | **8k** |
| `proxy-buffering` | off | on | **off** |
| `proxy-buffer-size` | 4k | not set (nginx: one page) | **4k** |
| `proxy-buffers-number` (annotation only) | 4 (`proxy_buffers 4 4k`) | not set (nginx: `8 4k`) | **`4 4k`** |
| `proxy-request-buffering` (annotation only) | on | not set (nginx: on) | on |
| `proxy-next-upstream` (annotation only) | error timeout | not set (nginx: error timeout) | error timeout |
| `proxy-next-upstream-timeout` (annotation only) | 0 | not set (nginx: 0) | 0 |
| `proxy-next-upstream-tries` (annotation only) | 3 | not set (nginx: 0, every server) | **3** |
| `proxy-redirect-from` (annotation only) | off | not set (nginx: `proxy_redirect default`) | **off** |
| `upstream-keepalive-connections` | 320 | 0, no `keepalive` | **64** per Service port (see below) |
| `http-redirect-code` | 308 | 301 | **308** |
| `hsts` | true | false | **true** |
| `hsts-include-subdomains` | true | false | **true** |
| `hsts-max-age` | 31536000 | 2592000 | **31536000** |
| `hsts-preload` | false | always `preload` | **false** |
| `use-http2` | true | false | **true** |
| `server-tokens` | false | on | **off** |
| `ssl-protocols` | TLSv1.2 TLSv1.3 | not set (nginx: the same) | TLSv1.2 TLSv1.3 |
| `ssl-ciphers` | ECDHE/DHE AES-GCM and CHACHA20 list, `ssl_prefer_server_ciphers on` | not set (nginx: `HIGH:!aNULL:!MD5`) | **ingress-nginx's list, prefer on** |
| `max-worker-connections` | 16384 | 1024 | **16384** |
| `worker-shutdown-timeout` | 240s | not set | **240s** |
| `variables-hash-max-size` | 2048 | 1024 | **2048** |
| `limit-req-status-code` (no effect as a key) | 503 | 429 for `nginx.org/limit-req-*` (ingress-nginx's rate limits answer 503 anyway) | **503** |
| `generate-request-id` (no effect as a key) | true: `X-Request-ID` to the upstream, the client's or `$request_id` | not sent | **sent** |
| `compute-full-forwarded-for` (no effect as a key) | false: `X-Forwarded-For $remote_addr` | `$proxy_add_x_forwarded_for` | **`$remote_addr`** |
| WebSocket (not a setting: always in ingress-nginx) | `Upgrade` and `Connection` in every location | only for `nginx.org/websocket-services` | **every Ingress location** |
| Other headers to the upstream | `X-Scheme`, `X-Forwarded-Scheme`, `X-Original-Forwarded-For`, `Proxy ""` | not sent | **sent** |
| The same path twice for a host of one Ingress (not a setting: a host listed twice, a path listed twice) | the first one's location | two `location` of the path, a configuration nginx refuses (`duplicate location`) for every resource | **the first one**, as ingress-nginx |
| The port of an Ingress backend (not a setting) | a port of the Service by its number or name, or else by its `targetPort` (a backend's 5000 is the Service's port 80 to 5000) | the port's number or name only: "no port 5000 in service", 502 | **also the `targetPort`**, a port with that number or name first |

`upstream-keepalive-connections`: in ingress-nginx the 320 are the idle
connections one worker keeps for all backends together, in the single
upstream of its Lua balancer. In wxingress every Service port is an upstream
of its own with its own `keepalive`, and nginx allocates each one's cache
whole when it reads the configuration - some 140 bytes an entry, 45 kB an
upstream at 320, 5 MB of every pod for a hundred Ingresses. So the default is
64 idle connections per worker and Service port: a Service that has more
than 64 requests in flight on one worker opens a connection for each beyond
them, where ingress-nginx would keep up to 320 open. Setting the key in the
ConfigMap gives every upstream that number.

### The same either way

| Setting | ingress-nginx | wxingress |
|---|---|---|
| `proxy-read-timeout`, `proxy-send-timeout` | 60s | 60s |
| `proxy-body-size` | 1m | 1m |
| `proxy-busy-buffers-size` | not set | not set |
| `proxy-max-temp-file-size` | 1024m | not set (nginx: 1024m) |
| `proxy-http-version` (annotation only) | 1.1 | not set (nginx 1.31: 1.1); left unset so gRPC and `appProtocol: kubernetes.io/h2c` keep their own |
| `proxy-cookie-domain`, `proxy-cookie-path` (annotations only) | off | not set (nginx: off) |
| `ssl-redirect` | true | true |
| `keep-alive`, `keep-alive-requests` | 75s, 1000 | 75s, 1000 |
| `worker-processes` | number of CPUs | auto |
| `variables-hash-bucket-size` | 256 | 256 |
| `server-name-hash-max-size` | 1024 | 1024 |
| `error-log-level` | notice | notice |
| `use-forwarded-headers` | false | false |
| `ssl-early-data`, `ssl-ecdh-curve` | off, auto | not set (nginx: off, auto) |
| `client-header-buffer-size`, `large-client-header-buffers`, `client-header-timeout`, `client-body-timeout` | 1k, 4 8k, 60s, 60s | not set (nginx: the same) |
| `http2-max-concurrent-streams` | 128 | not set (nginx: 128) |
| `enable-underscores-in-headers`, `ignore-invalid-headers` | off, on | not set (nginx: the same) |
| `limit-conn-status-code` (not a key) | 503 | 503 for ingress-nginx's `limit-connections` |
| default backend | 404 (`--default-backend-service`) | 404 (`-default-backend-service`) |

### Not covered

These defaults of ingress-nginx differ from what wxingress renders and the
switch does not change them, because wxingress does not render the directive
or the difference is one of capacity, not of behaviour:

| Setting | ingress-nginx | wxingress |
|---|---|---|
| `ssl-session-cache`, `ssl-session-cache-size`, `ssl-session-timeout` | `shared:SSL:10m`, 10m | nginx's: no shared cache, 5m |
| `ssl-session-tickets` | off | nginx's: on; the key is taken without a warning and has no effect |
| `ssl-buffer-size` | 4k | nginx's: 16k |
| `upstream-keepalive-time`, `-timeout`, `-requests` | 1h, 60s, 10000 | nginx's: 1h, 60s, 1000 |
| `map-hash-bucket-size` | 64 | 256 (larger, only capacity) |
| `server-names-hash-bucket-size` | computed from the longest name | 256 |
| `log-format-upstream` | its upstreaminfo format | wxingress's `main` format |
| `default-type` | text/html | application/octet-stream |
| `use-gzip`, `enable-brotli` | off | off |
| `reuse-port`, `aio threads`, `tcp_nopush`, `reset_timedout_connection`, `log_subrequest`, `port_in_redirect off`, `types_hash_max_size 2048` | on | nginx's |
| `X-Forwarded-Host`, `Host` | `$http_host` (with the client's port) | `$host` |
| `X-Original-Forwarded-Host` | the client's `X-Forwarded-Host` | not sent |
| SSL redirect | `https://host` and the URI without its trailing slash | `https://host:443` and the URI as it is |
| headers of gRPC locations | the same set as for HTTP | wxingress's gRPC headers |
| `proxy-stream-*` (TCP/UDP services) | 600s, 3 tries | the same for `-tcp-services-configmap`; TransportServers have their own |
| ConfigMap keys of ingress-nginx | read | with the switch translated into those of wxingress (above) |
