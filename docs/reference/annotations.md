# Annotations

wxingress reads the `nginx.ingress.kubernetes.io/*` annotations of ingress-nginx with the semantics of ingress-nginx,
so that an Ingress written for ingress-nginx behaves the same without changes. It also keeps the `nginx.org/*`
annotations of NGINX Ingress Controller, on which it is built. This page lists which ingress-nginx annotations are
supported, which behave differently, and which are not supported. Names below are given without the
`nginx.ingress.kubernetes.io/` prefix.

## General rules

- An invalid value is ignored, with a Warning event on the Ingress, as ingress-nginx ignores it. Values are trimmed
  line by line first.
- Annotations that take nginx variables take only those every server knows: `$1` to `$9` (the captures of the path's regular expression), the request's `$http_<header>`, `$arg_<name>` and `$cookie_<name>`, and `$args`, `$binary_remote_addr`, `$content_length`, `$content_type`, `$document_root`, `$document_uri`, `$escaped_request_uri`, `$fastcgi_path_info`, `$fastcgi_script_name`, `$host`, `$hostname`, `$https`, `$is_args`, `$proxy_add_x_forwarded_for`, `$proxy_host`, `$proxy_port`, `$query_string`, `$realpath_root`, `$remote_addr`, `$remote_port`, `$remote_user`, `$request_filename`, `$request_id`, `$request_method`, `$request_uri`, `$scheme`, `$server_addr`, `$server_name`, `$server_port`, `$server_protocol` and `$uri`. Any other
  variable (also `$best_http_host`, which wxingress does not define) makes the value invalid, since nginx would refuse
  the whole configuration. Regular expressions nginx compiles (paths with `use-regex` or `rewrite-target`, `~` values
  of `proxy-redirect-from` and `proxy-cookie-*`) must be valid PCRE; .NET-only syntax such as balancing groups is
  refused.
- Where an `nginx.org/*` annotation sets the same thing, the `nginx.org/*` annotation wins and the ingress-nginx one
  is ignored with a warning (see [nginx.org annotations](#nginxorg-annotations)).
- `nginx.org/*`, `nginx.com/*` and `appprotect.f5.com/*` annotations are validated differently: most of them are
  checked before the Ingress is configured, and an invalid value - or one that needs NGINX Plus, App Protect or
  `-enable-snippets` - rejects the whole Ingress (Warning event `Rejected`) instead of being ignored.
- Where ingress-nginx refuses to serve a location because its protection cannot be set up (a missing auth Secret, an
  invalid `auth-url`, an invalid source range, a missing ConfigMap), the locations of the Ingress answer **503** instead
  of serving without it. The Warning event names the reason.
- An ingress-nginx annotation that wxingress does not know, typos included, is ignored with a Warning event
  (`... is not supported by wxingress and is ignored`).
- Annotations that only exist to restrict access and that wxingress does not implement make every location of the
  Ingress answer **503**, see [Not supported](#not-supported).
- Resources of other namespaces (Secrets, ConfigMaps) are never used, as ingress-nginx with
  `allow-cross-namespace-resources: false`; naming one makes the locations answer 503.
- Snippet annotations (`server-snippet`, `configuration-snippet`, `stream-snippet`, `auth-snippet`) need
  `-enable-snippets` (Helm `controller.enableSnippets`), the equivalent of ingress-nginx's `allow-snippet-annotations`.
  Without it they are ignored with a warning, and the [admission webhook](helm-values.md#admission-webhook) refuses
  them. wxingress has no Lua: snippets with `*_by_lua*` directives must be rewritten (for example in njs).

Which Ingresses are served at all is decided by their class, see `-ingress-class-controllers` and
`-watch-ingress-without-class` in [Command-line flags](cli-flags.md#which-ingresses-are-served). The
`kubernetes.io/ingress.class` annotation wins over `spec.ingressClassName`, as in ingress-nginx.

## Supported

These behave as in ingress-nginx.

| Annotations | Notes |
|---|---|
| `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` | Whole seconds, rendered with `s`. |
| `proxy-body-size`, `client-body-buffer-size` | `client_max_body_size` (k, m, g) and `client_body_buffer_size` (k, m). |
| `proxy-buffering`, `proxy-request-buffering` | `on` or `off`. |
| `proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size` | Rendered as ingress-nginx renders them: `proxy_buffer_size S` and `proxy_buffers N S`, so either of the first two sets both (N defaults to 4, S to `4k` or the ConfigMap's). |
| `proxy-max-temp-file-size`, `proxy-http-version` | `proxy-http-version` takes `1.0` or `1.1`. |
| `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries` | ingress-nginx's validation (`error`, `timeout`, `invalid_header`, `http_500`, `http_502`, `http_503`, `http_504`, `http_403`, `http_404`, `http_429`, `non_idempotent`, `off`); `non_idempotent` is moved to the end. The timeout is whole seconds. |
| `proxy-redirect-from`, `proxy-redirect-to` | Validated as `nginx.org/proxy-redirect-from` and `-to`; one without the other is ignored (except `off` and `default`). |
| `proxy-cookie-domain`, `proxy-cookie-path` | `proxy_cookie_domain`, `proxy_cookie_path`: `<from> <to>` or `off`. |
| `rewrite-target`, `use-regex`, `x-forwarded-prefix` | A `rewrite-target` or `use-regex` turns every path of the host (of every Ingress on it) into a case-insensitive regular expression, ordered longest first, as ingress-nginx does. `x-forwarded-prefix` sends `X-Forwarded-Prefix` only from locations that `rewrite-target` rewrites. |
| `app-root`, `upstream-vhost`, `server-alias` | A `server-alias` wildcard must leave at least two labels (`*.example.com`, not `*.com`). |
| `permanent-redirect`, `permanent-redirect-code`, `temporal-redirect`, `temporal-redirect-code` | `temporal-redirect` wins. Default codes 301 and 302; a code outside 300-308 is the default, with a warning. The URL must be http or https. |
| `ssl-ciphers`, `ssl-prefer-server-ciphers` | |
| `backend-protocol` | `HTTP`, `HTTPS`, `GRPC`, `GRPCS`, `AUTO_HTTP`, `FCGI` (case-insensitive). Any other value (`AJP`) makes the locations answer 503. |
| `fastcgi-index`, `fastcgi-params-configmap` | With `backend-protocol: FCGI`; without it ignored with a warning. A missing ConfigMap, one of another namespace or an invalid parameter: 503. |
| `service-upstream` | Proxy to the Service's ClusterIP instead of its endpoints; a headless Service to its endpoints. |
| `enable-cors`, `cors-allow-origin`, `cors-allow-methods`, `cors-allow-headers`, `cors-allow-credentials`, `cors-expose-headers`, `cors-max-age` | ingress-nginx's defaults (origin `*`, methods `GET, PUT, POST, DELETE, PATCH, OPTIONS`, its header list, credentials `true`, max age `1728000`) and validation; preflight requests of allowed origins are answered with 204 before auth. |
| `allowlist-source-range`, `whitelist-source-range`, `denylist-source-range` | IPv4 and IPv6 addresses and CIDRs. `allowlist-source-range` wins over `whitelist-source-range`. An invalid range makes the locations answer 503. |
| `limit-rps`, `limit-rpm`, `limit-connections`, `limit-burst-multiplier`, `limit-allowlist`, `limit-whitelist`, `limit-rate`, `limit-rate-after` | ingress-nginx's zones (5m, keyed by `$binary_remote_addr`), bursts (limit times `limit-burst-multiplier`, default 5, `nodelay`), allowlist and its 503. `limit-allowlist` wins over `limit-whitelist`. `limit-rate` and `limit-rate-after` are kilobytes (`limit_rate <n>k`). An invalid limit or allowlist drops the whole rate limit, as in ingress-nginx. Limits are per controller pod, as in ingress-nginx. |
| `auth-type`, `auth-secret`, `auth-secret-type`, `auth-realm` | Basic auth from an Opaque Secret (`auth-file`, the default, with key `auth`, or `auth-map`), updated when the Secret changes; the Authorization header is not passed on. A missing or invalid Secret, a missing `auth-secret` or another `auth-type` than `basic` makes the locations answer 503. |
| `auth-url`, `auth-method`, `auth-signin`, `auth-signin-redirect-param`, `auth-response-headers`, `auth-request-redirect`, `auth-cache-key`, `auth-cache-duration`, `auth-always-set-cookie`, `auth-snippet` | External auth with `auth_request`, as in ingress-nginx: `auth-signin-redirect-param` defaults to `rd`, `auth-cache-duration` to `200 202 401 5m` (used with `auth-cache-key`). An invalid `auth-url`, `auth-method`, `auth-response-headers`, `auth-request-redirect`, `auth-always-set-cookie` or `auth-cache-duration` makes the locations answer 503. `auth-snippet` needs `-enable-snippets`. |
| `auth-proxy-set-headers` | A ConfigMap of the Ingress's namespace; watched. |
| `auth-tls-secret`, `auth-tls-verify-client`, `auth-tls-verify-depth`, `auth-tls-error-page`, `auth-tls-pass-certificate-to-upstream`, `auth-tls-match-cn` | Client certificates from `ca.crt` (and `ca.crl`) of the Secret: `auth-tls-verify-client` `on` (default), `off`, `optional` or `optional_no_ca`; depth 1 by default; `auth-tls-match-cn` is `CN=<regular expression>`. A missing Secret or an invalid `auth-tls-match-cn` makes the locations answer 503. |
| `proxy-ssl-secret`, `proxy-ssl-verify`, `proxy-ssl-verify-depth`, `proxy-ssl-ciphers`, `proxy-ssl-protocols`, `proxy-ssl-name`, `proxy-ssl-server-name` | TLS to the backends, with ingress-nginx's defaults. |
| `connection-proxy-header` | `close` or `keep-alive`. |
| `enable-access-log`, `enable-rewrite-log` | `enable-access-log: "false"` is `access_log off`, `enable-rewrite-log: "true"` is `rewrite_log on`. |
| `relative-redirects`, `use-port-in-redirects` | `relative-redirects: "true"` is `absolute_redirect off`; `use-port-in-redirects` is `port_in_redirect on` or `off`. |
| `satisfy` | `satisfy any` or `satisfy all` in the locations of the Ingress; another value is ignored with a warning. |
| `default-backend`, `custom-http-errors`, `disable-proxy-intercept-errors` | See also `-default-backend-service`. `disable-proxy-intercept-errors: "true"` stops intercepting the `custom-http-errors` codes; a `default-backend` still gets the 502 and 503. |
| `mirror-target`, `mirror-host`, `mirror-request-body` | `mirror-request-body: off` does not mirror the body (default `on`); `mirror-host` defaults to the target's host. |
| `ssl-passthrough` | Needs `-enable-tls-passthrough` (Helm `controller.enableTLSPassthrough`), the equivalent of ingress-nginx's `--enable-ssl-passthrough`; without it ignored with a warning, as in ingress-nginx. |
| `server-snippet`, `configuration-snippet`, `stream-snippet` | Need `-enable-snippets`. |

## Supported with differences

| Annotations | Difference from ingress-nginx |
|---|---|
| `ssl-redirect`, `force-ssl-redirect`, `preserve-trailing-slash` | The redirect is the same (`https://<host><URI>`, 308 with `-ingress-nginx-defaults`), but whether a host redirects is decided per server, by the first Ingress of the host; ingress-nginx decides per location. |
| `affinity: cookie`, `affinity-mode`, `affinity-canary-behavior`, `session-cookie-name`, `session-cookie-path`, `session-cookie-domain`, `session-cookie-expires`, `session-cookie-max-age`, `session-cookie-secure`, `session-cookie-samesite`, `session-cookie-conditional-samesite-none`, `session-cookie-change-on-failure` | With `-dynamic-upstreams`: ingress-nginx's sticky balancers in the wxingress module (`affinity-mode` `balanced` or `persistent`, `affinity-canary-behavior` `sticky` or `legacy`), with the same cookie format, so existing sessions stay pinned. The cookie is `INGRESSCOOKIE` by default, its path the common prefix of the Ingress's paths. Without it: NGINX's `sticky cookie`, HttpOnly, and `session-cookie-change-on-failure` is always true. An `nginx.org/sticky-cookie-services` cookie of the same Service wins. |
| `load-balance`, `upstream-hash-by`, `upstream-hash-by-subset`, `upstream-hash-by-subset-size` | `load-balance` takes `round_robin` or `ewma`; `upstream-hash-by-subset-size` defaults to 3; with `affinity` both are ignored. With `-dynamic-upstreams`: ingress-nginx's `round_robin`, `ewma`, `chash` and `chashsubset` in the wxingress module; changing them needs no reload. Small differences: subsets are built from servers sorted by address (the same on every replica), and `ewma` does not average in failed tries. Without it: `hash ... consistent`, `ewma` becomes `least_time last_byte`, and `upstream-hash-by-subset` hashes to a single endpoint, with a warning. |
| `canary`, `canary-weight`, `canary-weight-total`, `canary-by-header`, `canary-by-header-value`, `canary-by-header-pattern`, `canary-by-cookie` | Need `-dynamic-upstreams`; without it, or with an invalid canary annotation, the canary Ingress is ignored with an event. `canary-weight-total` defaults to 100. With `-ingress-nginx-defaults` a canary attaches to the upstream of the matching host and path in any Ingress, as ingress-nginx's alternative backend (only in its own namespace unless `-ingress-nginx-cross-namespace`); without it, only to the location of the Ingress with the same host and path, not to other Ingresses that use the same Service. Weight, header and cookie changes need no reload; a canary that attaches to an upstream or leaves one reloads NGINX (ingress-nginx needs none). |
| `auth-type: digest` | Not available in open-source NGINX: the locations answer 503. |
| `auth-tls-*` | On a host without TLS: with `-ingress-nginx-defaults`, HTTPS with the default certificate checks the client certificate, but plain HTTP answers 503 (ingress-nginx serves it without the check). Without the defaults, the locations answer 503. An `nginx.org` IngressMTLS Policy wins. |
| `auth-keepalive`, `auth-keepalive-requests`, `auth-keepalive-timeout`, `auth-keepalive-share-vars` | `auth-keepalive` idle connections to the auth service (default 0, off), with 1000 requests and 60 s by default. As in ingress-nginx not used with HTTP/2 on (ignored with a warning) or when the host of `auth-url` has a variable. `auth-keepalive-share-vars` is what NGINX's subrequests do anyway. |
| `custom-headers` | A ConfigMap of the Ingress's namespace, each header listed in the ConfigMap key `global-allowed-response-headers`. Rendered with `add_header ... always` and `add_header_inherit merge` instead of `more_set_headers`, so the server's own headers stay. |
| `enable-cors` | Rendered with `add_header ... always` instead of `more_set_headers`. |
| `custom-http-errors` | Also a 502 or 503 a backend sends itself goes to the `default-backend`, not only NGINX's own. Without `default-backend` the codes go to `-default-backend-service`; without either the annotation is ignored with a warning. |
| `from-to-www-redirect` | Not applied when another resource has the other name. |
| `enable-opentelemetry`, `opentelemetry-trust-incoming-span`, `opentelemetry-operation-name` | With NGINX's otel module; traces go only to the ConfigMap's `otel-exporter-endpoint` (ingress-nginx's `otlp-collector-host`). Without one the annotations are ignored with a warning. |
| `proxy-ssl-*` | An `nginx.org` egressMTLS Policy wins. A Secret with neither certificate nor CA makes the locations answer 503. |
| `mirror-target` | The target is resolved per request with the pod's name servers. |
| `stream-snippet` | One file of the `stream` block per Ingress, removed with the Ingress. |

## Not supported

Every ingress-nginx annotation not listed above is ignored with a Warning event. Among them are `enable-global-auth`
(wxingress has no global auth; ignored with a warning whatever its value, it does not make the locations answer 503), `enable-opentracing` and `opentracing-trust-incoming-span` (use OpenTelemetry), and
`http2-push-preload`.

Annotations that restrict access and that wxingress does not implement are not ignored: an Ingress with one of them
answers **503** on all its locations, with a Warning event, because serving it without the restriction would serve what
ingress-nginx refuses.

| Annotations | Answers 503 when |
|---|---|
| `enable-modsecurity`, `enable-owasp-core-rules` | set to anything but `false` |
| `modsecurity-snippet` | set |
| `global-rate-limit` | set to anything but `0` |
| `modsecurity-transaction-id`, `global-rate-limit-window`, `global-rate-limit-key`, `global-rate-limit-ignored-cidrs` | never on their own (they only qualify the annotations above); otherwise ignored with a warning |

!!! warning
    Find these before switching with `kubectl get ingress -A -o yaml | grep -E 'modsecurity|global-rate-limit'`.
    Rate limits per pod (`limit-rps`, `limit-rpm`) or a rate-limit Policy can replace a global rate limit.

## Gateway API routes

With `-enable-gateway-api`, HTTPRoutes and GRPCRoutes read a subset of the same annotations, with the same code as for
Ingresses: `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout`, `proxy-body-size`,
`client-body-buffer-size`, `proxy-buffering`, `proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size`,
`proxy-http-version`, `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries`,
`service-upstream`, `load-balance`, `upstream-hash-by`, `upstream-hash-by-subset`, `upstream-hash-by-subset-size`,
`affinity`, `affinity-mode`, the `session-cookie-*` annotations, `limit-rps`, `limit-rpm`, `limit-connections`,
`limit-burst-multiplier`, `limit-allowlist` (`limit-whitelist`), `limit-rate`, `limit-rate-after`,
`canary-by-header`, `canary-by-header-value`, `canary-by-header-pattern`, `canary-by-cookie`, `backend-protocol`, and
with `-enable-snippets` `server-snippet`, `configuration-snippet`, `nginx.org/server-snippets` and
`nginx.org/location-snippets` (without it they are ignored with a warning; an Ingress with the `nginx.org` ones is
rejected instead).

On a route, `backend-protocol` takes `HTTP`, `HTTPS`, `GRPC`, `GRPCS` and `AUTO_HTTP`; `HTTPS` and `GRPCS` turn on TLS
to the backends, which an `egressMTLS` Policy can give a client certificate and verification. The `canary-by-*`
annotations need `-dynamic-upstreams`, an HTTPRoute and a rule without a mirror: the rule's second backend is the
canary, and the backends' weights are the canary's weight. `proxy-request-buffering`, `proxy-max-temp-file-size` and
`affinity-canary-behavior` are accepted on a route without a warning but have no effect there.

Every other ingress-nginx annotation on a route is reported as a warning in the route's status and events and
ignored, except those that restrict access: `auth-*`, `allowlist-source-range`, `whitelist-source-range`,
`denylist-source-range`, `satisfy`, `enable-modsecurity` and `enable-owasp-core-rules` (other than `false`),
`modsecurity-snippet` and `global-rate-limit` (other than `0`). As on an Ingress, a route is not served without them:
all its rules answer **503**. Restrict routes with Policies attached through
`wxingress.wizardtales.com/policies: <policy>[, <policy>...]` on the route or the Gateway. Redirects, rewrites, header
changes and CORS are the Gateway API's own filters.

## nginx.org annotations

The annotations of NGINX Ingress Controller keep working as upstream defines them; see the
[NGINX Ingress Controller annotation reference](https://docs.nginx.com/nginx-ingress-controller/configuration/ingress-resources/advanced-configuration-with-annotations/)
for details. Unlike the ingress-nginx annotations, most of them are validated before the Ingress is configured, and an
invalid value rejects the whole Ingress. Where a ConfigMap key of the same name exists, the annotation overrides it for
the Ingress, and the default is the ConfigMap's (see [ConfigMap](configmap.md)).

| Annotation | Description |
|---|---|
| `nginx.org/mergeable-ingress-type` | `master` or `minion`, see below. |
| `nginx.org/policies` | Comma-separated Policies (`<name>` or `<namespace>/<name>`) applied to the Ingress. |
| `nginx.org/proxy-connect-timeout`, `nginx.org/proxy-read-timeout`, `nginx.org/proxy-send-timeout` | `proxy_connect_timeout`, `proxy_read_timeout`, `proxy_send_timeout` (nginx time, a bare number is seconds). |
| `nginx.org/client-max-body-size`, `nginx.org/client-body-buffer-size` | `client_max_body_size`, `client_body_buffer_size`. |
| `nginx.org/proxy-buffering`, `nginx.org/proxy-buffers`, `nginx.org/proxy-buffer-size`, `nginx.org/proxy-busy-buffers-size`, `nginx.org/proxy-max-temp-file-size` | `proxy_buffering` (`true`/`false`), `proxy_buffers` (`8 4k`), `proxy_buffer_size`, `proxy_busy_buffers_size`, `proxy_max_temp_file_size`. |
| `nginx.org/proxy-hide-headers`, `nginx.org/proxy-pass-headers` | Comma-separated headers for `proxy_hide_header` and `proxy_pass_header`. |
| `nginx.org/proxy-set-headers` | Comma-separated `Name: value` request headers to the upstream (`proxy_set_header`); a name without a value sends the client's header (`$http_<name>`). |
| `nginx.org/add-header`, `nginx.org/add-header-inherit` | Comma-separated `Name: value[: always]` response headers (`add_header`); `add_header_inherit` `on`, `off` or `merge`. |
| `nginx.org/proxy-http-version` | `proxy_http_version`. |
| `nginx.org/proxy-next-upstream`, `nginx.org/proxy-next-upstream-timeout`, `nginx.org/proxy-next-upstream-tries` | `proxy_next_upstream`, `proxy_next_upstream_timeout`, `proxy_next_upstream_tries`. |
| `nginx.org/proxy-redirect-from`, `nginx.org/proxy-redirect-to` | `proxy_redirect <from> <to>`; `from` alone only as `off` or `default`. |
| `nginx.org/upstream-vhost` | The Host header to the upstream. |
| `nginx.org/rewrites` | `serviceName=<svc> rewrite=<path>[;...]`: the path the requests to that Service are rewritten to. Wins over `nginx.org/rewrite-target`, with a warning. |
| `nginx.org/rewrite-target` | The path every location of the Ingress rewrites to, with `$1`-style captures of `nginx.org/path-regex` paths. |
| `nginx.org/path-regex` | `case_sensitive`, `case_insensitive` or `exact`: the paths of the Ingress as regular expressions or exact matches. |
| `nginx.org/app-root` | Redirect `/` to this path. |
| `nginx.org/ssl-services`, `nginx.org/grpc-services`, `nginx.org/websocket-services` | Comma-separated Services that are proxied with TLS, with gRPC, or with WebSocket support. |
| `nginx.org/use-cluster-ip` | `true`: proxy to the ClusterIP of the Services instead of their endpoints. |
| `nginx.org/lb-method` | The load-balancing method, as the ConfigMap's `lb-method`. |
| `nginx.org/keepalive`, `nginx.org/max-fails`, `nginx.org/max-conns`, `nginx.org/fail-timeout` | `keepalive` of the upstreams; `max_fails`, `max_conns` and `fail_timeout` of their servers. |
| `nginx.org/upstream-zone-size` | The shared memory zone of the upstreams. No effect with `-dynamic-upstreams`, where all upstreams share one zone. |
| `nginx.org/sticky-cookie-services` | `serviceName=<svc> <name> [expires=...] [domain=...] [httponly] [secure] [path=...][;...]`: NGINX's `sticky cookie` for those Services. |
| `nginx.org/ssl-redirect`, `ingress.kubernetes.io/ssl-redirect` | Redirect plain HTTP to HTTPS on a host with TLS (default the ConfigMap's `ssl-redirect`). |
| `nginx.org/redirect-to-https` | Redirect to HTTPS based on `X-Forwarded-Proto`. |
| `nginx.org/http-redirect-code` | 301, 302, 307 or 308 for the HTTPS redirect. |
| `nginx.org/hsts`, `nginx.org/hsts-max-age`, `nginx.org/hsts-include-subdomains`, `nginx.org/hsts-behind-proxy` | HSTS of the server; the other three apply only with `nginx.org/hsts`. |
| `nginx.org/http2` | HTTP/2 on the TLS listeners of the server. |
| `nginx.org/ssl-ciphers`, `nginx.org/ssl-prefer-server-ciphers` | `ssl_ciphers` and `ssl_prefer_server_ciphers` of the server. |
| `nginx.org/server-tokens` | `server_tokens` `on` or `off` (`true`/`false`). |
| `nginx.org/listen-ports`, `nginx.org/listen-ports-ssl` | Comma-separated ports the server listens on instead of 80 and 443; not on an Ingress without a host. |
| `nginx.org/basic-auth-secret`, `nginx.org/basic-auth-realm` | Basic auth from a Secret of type `nginx.org/htpasswd`; the realm only with the Secret. |
| `nginx.org/custom-http-errors` | Comma-separated codes or `4xx`/`5xx` of the upstreams sent to the Ingress's `spec.defaultBackend` (`proxy_intercept_errors`). |
| `nginx.org/limit-req-rate`, `nginx.org/limit-req-key`, `nginx.org/limit-req-zone-size`, `nginx.org/limit-req-burst`, `nginx.org/limit-req-delay`, `nginx.org/limit-req-no-delay`, `nginx.org/limit-req-dry-run`, `nginx.org/limit-req-log-level`, `nginx.org/limit-req-reject-code`, `nginx.org/limit-req-scale` | A request rate limit: rate `<n>r/s` or `<n>r/m`, key (default `${binary_remote_addr}`), zone size (default `10m`), burst, delay, nodelay, dry run, log level `info`, `notice`, `warn` or `error` (default `error`), reject code (default 429, 503 with `-ingress-nginx-defaults`), and with `limit-req-scale` the rate divided among the controller's replicas. |
| `nginx.org/server-snippets`, `nginx.org/location-snippets` | Lines for the server and the locations of the Ingress. Need `-enable-snippets`; without it the Ingress is rejected. They replace the ConfigMap's `server-snippets` and `location-snippets` for the Ingress. |

### NGINX Plus and App Protect only

These are read only with `-nginx-plus` or App Protect, which wxingress does not build. An Ingress with one of them is
rejected (`annotation requires NGINX Plus`, `annotation requires AppProtect`):

| Annotation | Upstream meaning |
|---|---|
| `nginx.com/health-checks`, `nginx.com/health-checks-mandatory`, `nginx.com/health-checks-mandatory-queue` | Active health checks. |
| `nginx.com/slow-start` | `slow_start` of the upstream servers. |
| `nginx.com/jwt-key`, `nginx.com/jwt-realm`, `nginx.com/jwt-token`, `nginx.com/jwt-login-url` | JWT validation; use a jwt Policy, which the wxingress module validates without NGINX Plus. |
| `nginx.com/sticky-cookie-services` | The NGINX Plus name of `nginx.org/sticky-cookie-services`, which works without it. |
| `nginx.com/policies` | The NGINX Plus name of `nginx.org/policies`. |
| `appprotect.f5.com/app-protect-enable`, `appprotect.f5.com/app-protect-policy`, `appprotect.f5.com/app-protect-security-log-enable`, `appprotect.f5.com/app-protect-security-log`, `appprotect.f5.com/app-protect-security-log-destination`, `appprotectdos.f5.com/app-protect-dos-resource` | NGINX App Protect WAF and DoS. |

Where an `nginx.org/*` annotation and an ingress-nginx annotation set the same thing on one Ingress, the `nginx.org/*`
annotation wins and the ingress-nginx annotation is ignored with a warning:

| ingress-nginx annotation | Overridden by |
|---|---|
| `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` | `nginx.org/proxy-connect-timeout`, `nginx.org/proxy-read-timeout`, `nginx.org/proxy-send-timeout` |
| `proxy-body-size`, `client-body-buffer-size` | `nginx.org/client-max-body-size`, `nginx.org/client-body-buffer-size` |
| `proxy-buffering`, `proxy-busy-buffers-size`, `proxy-max-temp-file-size` | `nginx.org/proxy-buffering`, `nginx.org/proxy-busy-buffers-size`, `nginx.org/proxy-max-temp-file-size` |
| `proxy-buffer-size`, `proxy-buffers-number` | Each by either `nginx.org/proxy-buffer-size` or `nginx.org/proxy-buffers` |
| `proxy-http-version`, `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries` | `nginx.org/proxy-http-version`, `nginx.org/proxy-next-upstream`, `nginx.org/proxy-next-upstream-timeout`, `nginx.org/proxy-next-upstream-tries` |
| `proxy-redirect-from`, `proxy-redirect-to` | `nginx.org/proxy-redirect-from`, `nginx.org/proxy-redirect-to` |
| `upstream-vhost`, `app-root`, `ssl-ciphers`, `ssl-prefer-server-ciphers`, `ssl-redirect` | `nginx.org/upstream-vhost`, `nginx.org/app-root`, `nginx.org/ssl-ciphers`, `nginx.org/ssl-prefer-server-ciphers`, `nginx.org/ssl-redirect` |
| `rewrite-target` | `nginx.org/rewrite-target`, `nginx.org/rewrites` |
| `use-regex` | `nginx.org/path-regex` |
| `service-upstream` | `nginx.org/use-cluster-ip` |
| `load-balance`, `upstream-hash-by` | `nginx.org/lb-method` |
| `affinity` | `nginx.org/sticky-cookie-services`, for the Services it names |
| `limit-rps`, `limit-rpm` | `nginx.org/limit-req-rate` |
| `auth-type` | `nginx.org/basic-auth-secret` |
| `server-snippet`, `configuration-snippet` | `nginx.org/server-snippets`, `nginx.org/location-snippets` |
| `backend-protocol` | `nginx.org/ssl-services` (for `HTTPS` and `GRPCS`), `nginx.org/grpc-services` (for `GRPC` and `GRPCS`) |
| `auth-tls-*`, `proxy-ssl-*` | An IngressMTLS or EgressMTLS Policy (`nginx.org/policies`) |

Masters and minions (`nginx.org/mergeable-ingress-type`) keep NGINX Ingress Controller's rules: the host belongs to
them, and other Ingresses of the host are rejected. A master ignores `nginx.org/rewrites`, `nginx.org/ssl-services`,
`nginx.org/grpc-services`, `nginx.org/websocket-services`, `nginx.org/sticky-cookie-services`,
`nginx.org/use-cluster-ip` and the health checks; a minion ignores the server-level annotations (`nginx.org/hsts`, `nginx.org/hsts-max-age`,
`nginx.org/hsts-include-subdomains`, `nginx.org/ssl-redirect`, `ingress.kubernetes.io/ssl-redirect`, `nginx.org/redirect-to-https`, `nginx.org/http-redirect-code`, `nginx.org/server-tokens`,
`nginx.org/listen-ports*`, `nginx.org/server-snippets`, `nginx.org/ssl-ciphers`, `nginx.org/ssl-prefer-server-ciphers`,
`nginx.org/http2`, `nginx.org/app-root`, `nginx.org/proxy-hide-headers`, `nginx.org/proxy-pass-headers`). Minions
inherit the master's timeouts, buffers, upstream, rate-limit, `nginx.org/location-snippets`,
`nginx.org/upstream-vhost` and `nginx.org/proxy-http-version` annotations unless they set their own. Regular Ingresses that share a host are merged into one server as
in ingress-nginx; see [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md).
