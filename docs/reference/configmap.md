# ConfigMap

The controller reads its NGINX settings from one ConfigMap, named with `-nginx-configmaps=<namespace>/<name>`. With the
Helm chart you set its keys in `controller.config.entries`. A change of the ConfigMap is applied without a restart; with
`-dynamic-upstreams` NGINX is reloaded only when the generated configuration actually changes. The keys are those of
NGINX Ingress Controller. With `-ingress-nginx-defaults` the ConfigMap also accepts the keys of ingress-nginx, which are
translated as described in [ingress-nginx keys](#ingress-nginx-keys).

```yaml
controller:
  config:
    entries:
      proxy-connect-timeout: "10s"
      client-max-body-size: "16m"
      hsts: "true"
```

An invalid value is ignored: the controller logs it and records a Warning event on the ConfigMap, and the default
stays in effect. Annotations on an Ingress override the ConfigMap for that Ingress.

## Defaults with and without `-ingress-nginx-defaults`

`-ingress-nginx-defaults` (Helm: `controller.ingressNginxDefaults` or `controller.ingressNginxCompat`) replaces the
defaults of NGINX Ingress Controller with those of ingress-nginx for everything neither the ConfigMap nor an annotation
sets. In the tables below, the column "Default" is the value without the flag and "ingress-nginx defaults" is the value
with it; an empty cell means it does not change. "not set" means wxingress does not render the directive, so NGINX's
own default applies.

## Keys of wxingress

### Timeouts and proxying

| Key | Default | ingress-nginx defaults | Description |
|---|---|---|---|
| `proxy-connect-timeout` | `60s` | `5s` | `proxy_connect_timeout`. A bare number is taken as seconds. |
| `proxy-read-timeout` | `60s` | | `proxy_read_timeout`. |
| `proxy-send-timeout` | `60s` | | `proxy_send_timeout`. |
| `client-max-body-size` | `1m` | | `client_max_body_size`. ingress-nginx's `proxy-body-size`. |
| `client-body-buffer-size` | not set | `8k` | `client_body_buffer_size`. |
| `proxy-buffering` | `true` | `false` | `proxy_buffering`. |
| `proxy-buffers` | not set | `4 4k` | `proxy_buffers`, for example `8 4k`. |
| `proxy-buffer-size` | not set | `4k` | `proxy_buffer_size`. With ingress-nginx defaults and no `proxy-buffers`, it also sizes the four buffers of `proxy_buffers`, as in ingress-nginx. |
| `proxy-busy-buffers-size` | not set | | `proxy_busy_buffers_size`. |
| `proxy-max-temp-file-size` | not set | | `proxy_max_temp_file_size`. |
| `proxy-hide-headers` | | | Comma-separated headers from the upstream not passed to the client (`proxy_hide_header`). |
| `proxy-pass-headers` | | | Comma-separated headers passed to the client that NGINX would hide (`proxy_pass_header`). |

With ingress-nginx defaults, Ingress locations also get ingress-nginx's retry settings (`proxy_next_upstream error
timeout`, 3 tries), `proxy_redirect off`, its headers to the upstream (`X-Request-ID`, `X-Scheme`,
`X-Forwarded-Scheme`, `X-Original-Forwarded-For`, `X-Forwarded-For $remote_addr`) and WebSocket support. These have no
ConfigMap key in wxingress; the [annotations](annotations.md) of ingress-nginx set them per Ingress.

### Upstreams

| Key | Default | ingress-nginx defaults | Description |
|---|---|---|---|
| `lb-method` | `random two least_conn` | | The load-balancing method of upstreams: `round_robin`, `least_conn`, `ip_hash`, `random`, `random two`, `random two least_conn`, `least_time header`, `least_time last_byte` (each optionally with `inflight`), or `hash <key> [consistent]` With `-dynamic-upstreams`, an Ingress with ingress-nginx's `load-balance`, `upstream-hash-by` or affinity annotations is balanced by the wxingress module instead. |
| `keepalive` | `0` (no `keepalive`) | `64` | Idle keepalive connections to each upstream, per worker. ingress-nginx's `upstream-keepalive-connections` (320 there, for all backends together; every Service port is its own upstream here). |
| `max-fails` | `1` | | `max_fails` of upstream servers. |
| `fail-timeout` | `10s` | | `fail_timeout` of upstream servers. |
| `upstream-zone-size` | `256k` | | The shared memory zone of each upstream. |

### Redirects, HSTS and HTTP/2

| Key | Default | ingress-nginx defaults | Description |
|---|---|---|---|
| `ssl-redirect` | `true` | | Redirect plain HTTP to HTTPS for hosts with TLS. |
| `redirect-to-https` | `false` | | Redirect to HTTPS based on `X-Forwarded-Proto`, for TLS terminated in front of wxingress. |
| `http-redirect-code` | `301` | `308` | The status code of the HTTPS redirect: 301, 302, 307 or 308. |
| `hsts` | `false` | `true` | Send `Strict-Transport-Security`. With ingress-nginx defaults the other `hsts-*` keys apply without `hsts` being set, as in ingress-nginx. |
| `hsts-max-age` | `2592000` | `31536000` | `max-age` of HSTS. |
| `hsts-include-subdomains` | `false` | `true` | `includeSubDomains`. |
| `hsts-preload` | `true` | `false` | `preload`. |
| `hsts-behind-proxy` | `false` | | Send HSTS based on `X-Forwarded-Proto`. |
| `http2` | `false` | `true` | HTTP/2 on TLS listeners. ingress-nginx's `use-http2`. |

### TLS

| Key | Default | ingress-nginx defaults | Description |
|---|---|---|---|
| `ssl-protocols` | not set (NGINX: `TLSv1.2 TLSv1.3`) | `TLSv1.2 TLSv1.3` | `ssl_protocols`. |
| `ssl-ciphers` | not set (NGINX: `HIGH:!aNULL:!MD5`) | ingress-nginx's ECDHE/DHE AES-GCM and CHACHA20 list | `ssl_ciphers`. |
| `ssl-prefer-server-ciphers` | `false` | `true` | `ssl_prefer_server_ciphers`. |
| `ssl-dhparam-file` | | | The content of a DH parameters file (PEM). |

### Client address and forwarded headers

| Key | Default | Description |
|---|---|---|
| `real-ip-header` | not set | `real_ip_header`, for example `X-Forwarded-For` or `proxy_protocol`. Ignored with TLS passthrough, which sets `proxy_protocol` itself. |
| `set-real-ip-from` | not set | Comma-separated trusted addresses or CIDRs (`set_real_ip_from`). |
| `real-ip-recursive` | `false` | `real_ip_recursive`. |
| `proxy-protocol` | `false` | Accept the PROXY protocol on the listeners. ingress-nginx's `use-proxy-protocol`. |
| `use-forwarded-headers` | `false` | Pass on the client's `X-Forwarded-*` headers instead of replacing them. Mutually exclusive with `disable-forwarded-headers`. |
| `disable-forwarded-headers` | `false` | Do not send `X-Forwarded-*` headers to upstreams. Requires `-enable-snippets`. |

### Headers

| Key | Default | Description |
|---|---|---|
| `add-header` | | Comma-separated `Name: value` response headers added at the `http` level. |
| `add-header-inherit` | not set | `add_header_inherit`: `on`, `off` or `merge`. |
| `global-allowed-response-headers` | | Comma-separated headers that ingress-nginx's `custom-headers` annotation may set, as the ingress-nginx key of the same name. |
| `server-tokens` | `true` (`on`) | `server_tokens`. ingress-nginx defaults: `off`. |

### Logging

| Key | Default | Description |
|---|---|---|
| `error-log-level` | `notice` | `error_log` level. |
| `access-log` | `/dev/stdout main` | Only a `syslog:` destination is accepted, for example `syslog:server=localhost:514`. |
| `access-log-off` | `false` | Turn the access log off. |
| `default-server-access-log-off` | `false` | Turn the access log of the default server off. |
| `log-format` | wxingress's `main` format | The HTTP access log format; several lines are joined. ingress-nginx's `log-format-upstream`. |
| `log-format-escaping` | `default` | `default`, `json` or `none`. |
| `stream-log-format`, `stream-log-format-escaping` | | The same for the stream (TCP/UDP) log. |

!!! note
    ingress-nginx's log variables (`$proxy_upstream_name`, `$req_id`, ...) do not exist in wxingress. A
    `log-format-upstream` taken over from ingress-nginx must be adapted, or NGINX refuses the configuration.

### Workers and hash tables

| Key | Default | ingress-nginx defaults | Description |
|---|---|---|---|
| `worker-processes` | `auto` | | `worker_processes`. |
| `worker-connections` | `1024` | `16384` | `worker_connections`. ingress-nginx's `max-worker-connections`. |
| `worker-rlimit-nofile` | not set | | `worker_rlimit_nofile`. ingress-nginx's `max-worker-open-files`. |
| `worker-cpu-affinity` | not set | | `worker_cpu_affinity`. |
| `worker-shutdown-timeout` | not set | `240s` | `worker_shutdown_timeout`. |
| `keepalive-timeout` | `75s` | | Client `keepalive_timeout`. ingress-nginx's `keep-alive`. |
| `keepalive-requests` | `1000` | | Client `keepalive_requests`. ingress-nginx's `keep-alive-requests`. |
| `server-names-hash-bucket-size` | `256` | | `server_names_hash_bucket_size`. |
| `server-names-hash-max-size` | `1024` | | `server_names_hash_max_size`. |
| `map-hash-bucket-size` | `256` | | `map_hash_bucket_size`. |
| `map-hash-max-size` | `2048` | | `map_hash_max_size`. |
| `variables-hash-bucket-size` | `256` | | `variables_hash_bucket_size`. |
| `variables-hash-max-size` | `1024` | `2048` | `variables_hash_max_size`. |

### Snippets and templates

| Key | Description |
|---|---|
| `main-snippets` | Lines for the `main` context. |
| `http-snippets` | Lines for the `http` context. |
| `server-snippets` | Lines for every `server` of Ingresses and VirtualServers. |
| `location-snippets` | Lines for every `location` of Ingresses and VirtualServers. |
| `stream-snippets` | Lines for the `stream` context. |
| `main-template`, `ingress-template`, `virtualserver-template`, `transportserver-template` | Replacement templates. See the warning on [custom templates](cli-flags.md#nginx-and-listeners). |

Snippets in the ConfigMap do not need `-enable-snippets`; only whoever can edit the ConfigMap can set them.

### Default server and status

| Key | Default | Description |
|---|---|---|
| `default-server-return` | `404` | What the default server answers for unknown hosts: a status code, or a code and text or URL, as in `return`. With `-default-backend-service`, that Service answers instead unless this key is set. |
| `external-status-address` | | The address written into the status of Ingresses (with `-report-ingress-status`); wins over `-external-service`. |

### OpenTelemetry

| Key | Default | Description |
|---|---|---|
| `otel-exporter-endpoint` | not set | The OTLP/gRPC collector, `host:port`. Without it no traces are exported. |
| `otel-exporter-header-name`, `otel-exporter-header-value` | | A header sent to the collector, for example for authentication. |
| `otel-service-name` | | The service name of the spans. |
| `otel-trace-in-http` | `false` | Trace every request (`otel_trace on` in the `http` context). |
| `otel-trace-context` | not set | `extract`, `inject`, `propagate` or `ignore` (`otel_trace_context`). |

### OIDC

`oidc-pkce-timeout` (`90s`), `oidc-id-tokens-timeout` (`1h`), `oidc-access-tokens-timeout` (`1h`),
`oidc-refresh-tokens-timeout` (`8h`), `oidc-sids-timeout` (`8h`) and the matching `*-zone-size` keys (`128K` for PKCE,
`1M` for the others) size and expire the key-value zones of OIDC Policies.

### NGINX Plus and App Protect only

These keys are inherited from NGINX Ingress Controller and only have an effect with `-nginx-plus` or App Protect,
which wxingress does not support: `resolver-addresses`, `resolver-ipv6`, `resolver-timeout`, `resolver-valid`,
`zone-sync`, `zone-sync-port`, `zone-sync-resolver-addresses`, `zone-sync-resolver-ipv6`, `zone-sync-resolver-valid`,
`app-protect-*`, `license-token-secret-name`, `ssl-certificate-secret-name`, `ssl-trusted-certificate-secret-name`,
`ssl-verify`, `enforce-initial-report` and `usage-report-*`. Without NGINX Plus the `resolver-*` keys are ignored
with a warning. For their meaning see the upstream
[ConfigMap reference](https://docs.nginx.com/nginx-ingress-controller/configuration/global-configuration/configmap-resource/).

!!! note "Keys without effect"
    `limit-req-status-code`, `max-conns` and `slow-start` are accepted without a warning but currently have no
    effect.

## ingress-nginx keys

With `-ingress-nginx-defaults` the ConfigMap also takes the keys of ingress-nginx, so that the `controller.config` of an
ingress-nginx Helm release can be used as it is. Without the flag only the keys of wxingress are read, and other keys
are ignored.

Every key that is not a key of wxingress is handled in one of the ways below. A key that is neither listed here nor
a wxingress key gets a Warning event on the ConfigMap (`<key> is not supported by wxingress and is ignored`).

### Taken under the same name

These ingress-nginx keys are also keys of wxingress and mean the same: `proxy-connect-timeout`, `proxy-read-timeout`,
`proxy-send-timeout`, `proxy-buffering`, `proxy-buffer-size`, `proxy-busy-buffers-size`, `proxy-max-temp-file-size`,
`client-body-buffer-size`, `ssl-redirect`, `http-redirect-code`, `hsts`, `hsts-include-subdomains`, `hsts-max-age`,
`hsts-preload`, `ssl-protocols`, `ssl-ciphers`, `server-tokens`, `use-forwarded-headers`, `error-log-level`,
`worker-processes`, `worker-cpu-affinity`, `worker-shutdown-timeout`, `map-hash-bucket-size`,
`variables-hash-bucket-size`, `variables-hash-max-size`, `otel-service-name`, `global-allowed-response-headers`.

### Renamed

| ingress-nginx key | wxingress key |
|---|---|
| `proxy-body-size` | `client-max-body-size` |
| `keep-alive` | `keepalive-timeout` (a bare number gets `s`) |
| `keep-alive-requests` | `keepalive-requests` |
| `upstream-keepalive-connections` | `keepalive` |
| `max-worker-connections` | `worker-connections` |
| `max-worker-open-files` | `worker-rlimit-nofile` |
| `use-proxy-protocol` | `proxy-protocol` (and the real client address from the PROXY header, see below) |
| `use-http2` | `http2` |
| `hide-headers` | `proxy-hide-headers` |
| `server-name-hash-max-size`, `server-name-hash-bucket-size` | `server-names-hash-max-size`, `server-names-hash-bucket-size` |
| `log-format-upstream`, `log-format-stream` | `log-format`, `stream-log-format` (with a warning: ingress-nginx's variables do not exist here) |
| `log-format-escape-json: "true"`, `log-format-escape-none: "true"` | `log-format-escaping: json`, `none` |
| `main-snippet`, `http-snippet`, `server-snippet`, `location-snippet`, `stream-snippet` | `main-snippets`, `http-snippets`, `server-snippets`, `location-snippets`, `stream-snippets` |
| `otlp-collector-host`, `otlp-collector-port` | `otel-exporter-endpoint` (`host:port`, port `4317` by default) |
| `enable-opentelemetry` | `otel-trace-in-http` |
| `opentelemetry-trust-incoming-span` | `otel-trace-context`: `propagate` (default, as ingress-nginx), `inject` for `"false"` |

Where the ConfigMap has both the ingress-nginx key and the wxingress key it is renamed to, the wxingress key wins and the
ingress-nginx key gets a warning.

### Client address: `enable-real-ip`

With `enable-real-ip: "true"` or `use-proxy-protocol: "true"`, the client address is taken as in ingress-nginx:

| ingress-nginx | wxingress |
|---|---|
| `use-proxy-protocol: "true"` | `real-ip-header: proxy_protocol` |
| `enable-real-ip: "true"` with `forwarded-for-header` | `real-ip-header: <forwarded-for-header>` (default `X-Forwarded-For`) |
| `proxy-real-ip-cidr` | `set-real-ip-from` (default `0.0.0.0/0`, as ingress-nginx) |

`real-ip-header` and `set-real-ip-from`, if set in the ConfigMap, win.

!!! warning
    As in ingress-nginx, `enable-real-ip` without `proxy-real-ip-cidr` trusts the header from every address. Set
    `proxy-real-ip-cidr` to the addresses of your load balancer.

### Allow and deny lists

`allowlist-source-range` (or the older `whitelist-source-range`; `allowlist-source-range` wins),
`denylist-source-range` and `block-cidrs` take comma-separated addresses and CIDRs (IPv4 and IPv6) and are rendered as
`deny` and `allow` rules in every server of Ingresses and VirtualServers: the denied ranges first, then the allowed
ones followed by `deny all`. NGINX's own servers (status, the module's API) are not affected. An Ingress with source
ranges of its own (annotations) uses those instead, as in ingress-nginx. A value that is not a list of addresses denies
every request, with a warning and an error in the log.

### Accepted without effect

These keys are accepted silently because wxingress behaves as their usual setting anyway:

| Key | Why |
|---|---|
| `allow-snippet-annotations`, `annotations-risk-level` | Snippets are controlled by `-enable-snippets`. |
| `enable-annotation-validations` | Annotations are always validated. |
| `strict-validate-path-type` | wxingress does not validate path types strictly. |
| `allow-cross-namespace-resources` | Secrets and ConfigMaps of other namespaces are never taken. |
| `no-tls-redirect-locations` | ACME challenges are never redirected. |
| `compute-full-forwarded-for` | With ingress-nginx defaults, `X-Forwarded-For` is `$remote_addr`, ingress-nginx's default (`false`). |
| `generate-request-id` | With ingress-nginx defaults, `X-Request-ID` is sent, ingress-nginx's default (`true`). |
| `ssl-session-tickets` | NGINX's default applies. |

!!! warning
    Because these keys are not read, a non-default value has no effect: `compute-full-forwarded-for: "true"`,
    `generate-request-id: "false"` and `ssl-session-tickets: "false"` behave as their ingress-nginx defaults (for
    `ssl-session-tickets`, NGINX's default `on`).

### Keys that restrict access: served without them

ingress-nginx keys that restrict who gets an answer and that wxingress does not implement are reported loudly: a
Warning event on the ConfigMap and an error in the controller log saying that the Ingresses are served **without**
them.

| Key | Instead |
|---|---|
| `global-auth-*` (`global-auth-url`, `global-auth-signin`, ...) | ingress-nginx's `auth-url` annotations per Ingress, or an external-auth Policy. |
| `enable-modsecurity`, `enable-owasp-modsecurity-crs`, `modsecurity-snippet` | No ModSecurity in wxingress. Use a WAF in front of it or in the application path. |
| `block-user-agents`, `block-referers` | A `map` in `http-snippets` with a check in `server-snippets`, or a WAF. |
| `global-rate-limit-*` | ingress-nginx's `limit-rps`/`limit-rpm` annotations (per controller pod), or a rate-limit Policy. |

!!! danger
    Check the controller log and the ConfigMap's events after the switch. An Ingress that relied on one of these keys
    for protection is reachable without it.

### Not supported

Every other ingress-nginx key is ignored with a Warning event. Common ones and what to do:

| Keys | Alternative |
|---|---|
| `proxy-next-upstream`, `proxy-next-upstream-timeout`, `proxy-next-upstream-tries`, `proxy-request-buffering`, `proxy-redirect-from`, `proxy-http-version`, `proxy-buffers-number`, `proxy-cookie-domain`, `proxy-cookie-path` | The annotations of the same name per Ingress. With ingress-nginx defaults the defaults of ingress-nginx apply (for example 3 tries, `proxy_redirect off`). |
| `load-balance`, `upstream-keepalive-time`, `-timeout`, `-requests` | `lb-method` and `keepalive` in the ConfigMap; `load-balance` per Ingress as an annotation. |
| `use-gzip`, `gzip-*`, `enable-brotli`, `brotli-*` | `http-snippets` (`gzip on;` ...). |
| `ssl-session-cache`, `ssl-session-cache-size`, `ssl-session-timeout`, `ssl-buffer-size`, `ssl-early-data`, `ssl-ecdh-curve`, `ssl-dh-param` | `http-snippets` for the directives; `ssl-dhparam-file` for DH parameters. |
| `client-header-buffer-size`, `client-header-timeout`, `client-body-timeout`, `large-client-header-buffers`, `http2-max-concurrent-streams`, `enable-underscores-in-headers`, `ignore-invalid-headers` | `http-snippets`. NGINX's defaults equal ingress-nginx's. |
| `limit-conn-status-code` | ingress-nginx's `limit-connections` already answers 503. |
| `custom-http-errors` | The `custom-http-errors` annotation per Ingress. |
| `add-headers`, `proxy-set-headers` | `add-header` in the ConfigMap; `custom-headers` per Ingress; `location-snippets`. |
| `access-log-path`, `error-log-path`, `disable-access-log`, `skip-access-log-urls`, `enable-syslog` | `access-log` (syslog) and `access-log-off`. |
| `force-ssl-redirect` | The `force-ssl-redirect` annotation per Ingress. |
| `default-type`, `reuse-port`, `enable-multi-accept`, `bind-address`, `proxy-stream-*` | Not configurable. |
| `use-geoip2`, `enable-opentracing`, `plugins`, `lua-shared-dicts` | Not available: wxingress has no Lua and no GeoIP2 module. |

See [ingress-nginx compatibility](../user-guide/ingress-nginx-compatibility.md) for how the defaults compare in detail.
