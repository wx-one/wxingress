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
| `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` | Whole seconds. |
| `proxy-body-size`, `client-body-buffer-size` | `client_max_body_size` (k, m, g) and `client_body_buffer_size` (k, m). |
| `proxy-buffering`, `proxy-request-buffering` | `on` or `off`. |
| `proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size` | Rendered as ingress-nginx renders them: `proxy_buffer_size S` and `proxy_buffers N S`. |
| `proxy-max-temp-file-size`, `proxy-http-version` | `proxy-http-version` takes `1.0` or `1.1`. |
| `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries` | ingress-nginx's validation; `non_idempotent` is moved to the end. |
| `proxy-redirect-from`, `proxy-redirect-to` | Validated as `nginx.org/proxy-redirect-from` and `-to`; one without the other is ignored (except `off` and `default`). |
| `proxy-cookie-domain`, `proxy-cookie-path` | `proxy_cookie_domain`, `proxy_cookie_path`: `<from> <to>` or `off`. |
| `rewrite-target`, `use-regex`, `x-forwarded-prefix` | A `rewrite-target` or `use-regex` turns every path of the host (of every Ingress on it) into a case-insensitive regular expression, ordered longest first, as ingress-nginx does. |
| `app-root`, `upstream-vhost`, `server-alias` | A `server-alias` wildcard must leave at least two labels (`*.example.com`, not `*.com`). |
| `permanent-redirect`, `permanent-redirect-code`, `temporal-redirect`, `temporal-redirect-code` | `temporal-redirect` wins. The URL must be http or https. |
| `ssl-ciphers`, `ssl-prefer-server-ciphers` | |
| `backend-protocol` | `HTTP`, `HTTPS`, `GRPC`, `GRPCS`, `AUTO_HTTP`, `FCGI`. Any other value makes the locations answer 503. |
| `fastcgi-index`, `fastcgi-params-configmap` | With `backend-protocol: FCGI`. |
| `service-upstream` | Proxy to the Service's ClusterIP instead of its endpoints. |
| `enable-cors`, `cors-allow-origin`, `cors-allow-methods`, `cors-allow-headers`, `cors-allow-credentials`, `cors-expose-headers`, `cors-max-age` | ingress-nginx's defaults and validation; preflight requests of allowed origins are answered with 204 before auth. |
| `allowlist-source-range`, `whitelist-source-range`, `denylist-source-range` | IPv4 and IPv6 addresses and CIDRs. `allowlist-source-range` wins over `whitelist-source-range`. An invalid range makes the locations answer 503. |
| `limit-rps`, `limit-rpm`, `limit-connections`, `limit-burst-multiplier`, `limit-allowlist`, `limit-whitelist`, `limit-rate`, `limit-rate-after` | ingress-nginx's zones, keys, bursts and its 503. Limits are per controller pod, as in ingress-nginx. |
| `auth-type`, `auth-secret`, `auth-secret-type`, `auth-realm` | Basic auth from an Opaque Secret (`auth-file` or `auth-map`), updated when the Secret changes. |
| `auth-url`, `auth-method`, `auth-signin`, `auth-signin-redirect-param`, `auth-response-headers`, `auth-request-redirect`, `auth-cache-key`, `auth-cache-duration`, `auth-always-set-cookie`, `auth-snippet` | External auth with `auth_request`, as in ingress-nginx. `auth-snippet` needs `-enable-snippets`. |
| `auth-proxy-set-headers` | A ConfigMap of the Ingress's namespace; watched. |
| `auth-tls-secret`, `auth-tls-verify-client`, `auth-tls-verify-depth`, `auth-tls-error-page`, `auth-tls-pass-certificate-to-upstream`, `auth-tls-match-cn` | Client certificates from `ca.crt` (and `ca.crl`) of the Secret. |
| `proxy-ssl-secret`, `proxy-ssl-verify`, `proxy-ssl-verify-depth`, `proxy-ssl-ciphers`, `proxy-ssl-protocols`, `proxy-ssl-name`, `proxy-ssl-server-name` | TLS to the backends, with ingress-nginx's defaults. |
| `connection-proxy-header` | `close` or `keep-alive`. |
| `enable-access-log`, `enable-rewrite-log` | `enable-access-log: "false"` is `access_log off`, `enable-rewrite-log: "true"` is `rewrite_log on`. |
| `relative-redirects`, `use-port-in-redirects` | `relative-redirects: "true"` is `absolute_redirect off`; `use-port-in-redirects` is `port_in_redirect on` or `off`. |
| `satisfy` | `satisfy any` or `satisfy all` in the locations of the Ingress; another value is ignored with a warning. |
| `default-backend`, `custom-http-errors`, `disable-proxy-intercept-errors` | See also `-default-backend-service`. `disable-proxy-intercept-errors: "true"` stops intercepting the `custom-http-errors` codes; a `default-backend` still gets the 502 and 503. |
| `mirror-target`, `mirror-host`, `mirror-request-body` | |
| `ssl-passthrough` | Needs `-enable-tls-passthrough` (Helm `controller.enableTLSPassthrough`), the equivalent of ingress-nginx's `--enable-ssl-passthrough`; without it ignored with a warning, as in ingress-nginx. |
| `server-snippet`, `configuration-snippet`, `stream-snippet` | Need `-enable-snippets`. |

## Supported with differences

| Annotations | Difference from ingress-nginx |
|---|---|
| `ssl-redirect`, `force-ssl-redirect`, `preserve-trailing-slash` | The redirect is the same (`https://<host><URI>`, 308 with `-ingress-nginx-defaults`), but whether a host redirects is decided per server, by the first Ingress of the host; ingress-nginx decides per location. |
| `affinity: cookie`, `affinity-mode`, `affinity-canary-behavior`, `session-cookie-*` | With `-dynamic-upstreams`: ingress-nginx's sticky balancers in the wxingress module, with the same cookie format, so existing sessions stay pinned. Without it: NGINX's `sticky cookie`, and `session-cookie-change-on-failure` is always true. |
| `load-balance`, `upstream-hash-by`, `upstream-hash-by-subset`, `upstream-hash-by-subset-size` | With `-dynamic-upstreams`: ingress-nginx's `round_robin`, `ewma`, `chash` and `chashsubset` in the wxingress module; changing them needs no reload. Small differences: subsets are built from servers sorted by address (the same on every replica), and `ewma` does not average in failed tries. Without it: `hash ... consistent`, `ewma` becomes `least_time last_byte`, and `upstream-hash-by-subset` hashes to a single endpoint, with a warning. |
| `canary`, `canary-weight`, `canary-weight-total`, `canary-by-header`, `canary-by-header-value`, `canary-by-header-pattern`, `canary-by-cookie` | Need `-dynamic-upstreams`. With `-ingress-nginx-defaults` a canary attaches to the upstream of the matching host and path in any Ingress, as ingress-nginx's alternative backend; without it, only to the location of the Ingress with the same host and path, not to other Ingresses that use the same Service. Weight, header and cookie changes need no reload; a canary that attaches to an upstream or leaves one reloads NGINX (ingress-nginx needs none). |
| `auth-type: digest` | Not available in open-source NGINX: the locations answer 503. |
| `auth-tls-*` | On a host without TLS: with `-ingress-nginx-defaults`, HTTPS with the default certificate checks the client certificate, but plain HTTP answers 503 (ingress-nginx serves it without the check). Without the defaults, the locations answer 503. An `nginx.org` IngressMTLS Policy wins. |
| `auth-keepalive`, `auth-keepalive-requests`, `auth-keepalive-timeout`, `auth-keepalive-share-vars` | As in ingress-nginx not used with HTTP/2 on (ignored with a warning) or when the host of `auth-url` has a variable. `auth-keepalive-share-vars` is what NGINX's subrequests do anyway. |
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
Ingresses: `proxy-*-timeout`, `proxy-body-size`, `client-body-buffer-size`, `proxy-buffering`, `proxy-buffer-size`,
`proxy-buffers-number`, `proxy-busy-buffers-size`, `proxy-next-upstream*`, `proxy-http-version`, `service-upstream`,
`load-balance`, `upstream-hash-by*`, `affinity`, `affinity-mode`, `session-cookie-*`, `limit-rps`, `limit-rpm`,
`limit-connections`, `limit-burst-multiplier`, `limit-allowlist`, `limit-rate`, `limit-rate-after`,
`canary-by-header`, `canary-by-header-value`, `canary-by-header-pattern`, `canary-by-cookie`, `backend-protocol`, and
with `-enable-snippets` `server-snippet`, `configuration-snippet`, `nginx.org/server-snippets` and
`nginx.org/location-snippets`.

Every other annotation on a route is reported as a warning in the route's status and events. Unlike on Ingresses, a
route is served without an annotation that restricts access; restrict routes with Policies attached through
`wxingress.wizardtales.com/policies: <policy>[, <policy>...]` on the route or the Gateway. Redirects, rewrites, header
changes and CORS are the Gateway API's own filters.

## nginx.org annotations

The annotations of NGINX Ingress Controller (`nginx.org/*`, and `nginx.com/*` for NGINX Plus) keep working. They are
inherited unchanged from upstream; see the
[NGINX Ingress Controller annotation reference](https://docs.nginx.com/nginx-ingress-controller/configuration/ingress-resources/advanced-configuration-with-annotations/)
for details. `nginx.com/*` annotations need NGINX Plus and do not apply to wxingress.

Where an `nginx.org/*` annotation and an ingress-nginx annotation set the same thing on one Ingress, the `nginx.org/*`
annotation wins and the ingress-nginx annotation is ignored with a warning:

| ingress-nginx annotation | Overridden by |
|---|---|
| `proxy-connect-timeout`, `proxy-read-timeout`, `proxy-send-timeout` | `nginx.org/proxy-connect-timeout`, `nginx.org/proxy-read-timeout`, `nginx.org/proxy-send-timeout` |
| `proxy-body-size`, `client-body-buffer-size` | `nginx.org/client-max-body-size`, `nginx.org/client-body-buffer-size` |
| `proxy-buffering`, `proxy-buffer-size`, `proxy-buffers-number`, `proxy-busy-buffers-size`, `proxy-max-temp-file-size` | `nginx.org/proxy-buffering`, `nginx.org/proxy-buffer-size`, `nginx.org/proxy-buffers`, `nginx.org/proxy-busy-buffers-size`, `nginx.org/proxy-max-temp-file-size` |
| `proxy-http-version`, `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries` | `nginx.org/proxy-http-version`, `nginx.org/proxy-next-upstream`, `nginx.org/proxy-next-upstream-timeout`, `nginx.org/proxy-next-upstream-tries` |
| `proxy-redirect-from`, `proxy-redirect-to` | `nginx.org/proxy-redirect-from`, `nginx.org/proxy-redirect-to` |
| `upstream-vhost`, `app-root`, `ssl-ciphers`, `ssl-prefer-server-ciphers`, `ssl-redirect` | `nginx.org/upstream-vhost`, `nginx.org/app-root`, `nginx.org/ssl-ciphers`, `nginx.org/ssl-prefer-server-ciphers`, `nginx.org/ssl-redirect` |
| `use-regex` | `nginx.org/path-regex` |
| `service-upstream` | `nginx.org/use-cluster-ip` |
| `load-balance`, `upstream-hash-by` | `nginx.org/lb-method` |
| `limit-rps`, `limit-rpm` | `nginx.org/limit-req-rate` |
| `auth-type` | `nginx.org/basic-auth-secret` |
| `server-snippet`, `configuration-snippet` | `nginx.org/server-snippets`, `nginx.org/location-snippets` |
| `backend-protocol` | `nginx.org/ssl-services`, `nginx.org/grpc-services` |
| `auth-tls-*`, `proxy-ssl-*` | An IngressMTLS or EgressMTLS Policy (`nginx.org/policies`) |

Masters and minions (`nginx.org/mergeable-ingress-type`) keep NGINX Ingress Controller's rules: the host belongs to
them, and other Ingresses of the host are rejected. Regular Ingresses that share a host are merged into one server as
in ingress-nginx; see [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md).
