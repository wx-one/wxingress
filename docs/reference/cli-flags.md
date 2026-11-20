# Command-line flags

The wxingress controller is configured with command-line flags on its container (`/nginx-ingress -flag=value`;
`--flag` works as well). The Helm chart sets them from its values, so with the chart you normally change a
[Helm value](helm-values.md) rather than a flag; the column "Helm value" names the value that sets each flag. Settings
of NGINX itself (timeouts, buffers, TLS, logging) are not flags but keys of the [ConfigMap](configmap.md).

Boolean flags take `-flag`, `-flag=true` or `-flag=false`. Values written as `<namespace>/<name>` refer to a
Kubernetes object. An invalid value of most flags stops the controller at startup with a message naming the flag.

!!! note "Defaults of the binary and of the chart"
    The defaults below are those of the controller binary. The Helm chart sets some of them differently, for
    example `-watch-ingress-without-class` (on in the chart) and `-report-ingress-status` (on in the chart). See
    [Helm values](helm-values.md) for the chart's defaults.

## Which Ingresses are served

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-ingress-class` | `nginx` | `controller.ingressClass.name` | The class of the controller. Ingresses whose `ingressClassName` (or `kubernetes.io/ingress.class` annotation) names it are served, whether or not an IngressClass of that name exists; a missing IngressClass, or one with another controller, only logs a warning. VirtualServers, VirtualServerRoutes and TransportServers without `ingressClassName` are served as well. |
| `-ingress-class-controllers` | `nginx.wx1.eu/ingress-controller` | `controller.ingressClass.controllerValue`, `controller.ingressClass.controllers` | Comma-separated list of IngressClass `spec.controller` values. The Ingresses of every IngressClass whose controller is in the list are served, whatever the class is called. Add `k8s.io/ingress-nginx` to serve the classes of ingress-nginx, beside it or in its place. Changes of IngressClasses take effect without a restart. |
| `-watch-ingress-without-class` | `false` | `controller.watchIngressWithoutClass` (chart default `true`) | Serve Ingresses without any class (no `ingressClassName`, no `kubernetes.io/ingress.class`), as ingress-nginx's `--watch-ingress-without-class`. Without it they are served only when a served IngressClass is the cluster's default. |
| `-watch-namespace` | all namespaces | `controller.watchNamespace` | Comma-separated list of namespaces to watch. Mutually exclusive with `-watch-namespace-label`. |
| `-watch-namespace-label` | (none) | `controller.watchNamespaceLabel` | Watch only the namespaces with this label selector, for example `foo=bar`. Mutually exclusive with `-watch-namespace`. |
| `-watch-secret-namespace` | the watched namespaces | `controller.watchSecretNamespace` | Comma-separated list of namespaces to watch for Secrets, if different from the namespaces watched for other resources. |
| `-allow-empty-ingress-host` | `false` | `controller.allowEmptyIngressHost` (on with `controller.ingressNginxCompat`) | Accept Ingresses without a `host`, including an Ingress with only `spec.defaultBackend` (a catch-all). TLS for them comes from `-default-server-tls-secret`, their ports from `-default-http-listener-port` and `-default-https-listener-port`. |
| `-enable-custom-resources` | `true` | `controller.enableCustomResources` | Serve the custom resources (VirtualServer, VirtualServerRoute, TransportServer, Policy, GlobalConfiguration). Required by the Gateway API, TLS passthrough, cert-manager and external-dns support. |
| `-global-configuration` | (none) | `controller.globalConfiguration.create`, `controller.globalConfiguration.customName` | The GlobalConfiguration resource (`<namespace>/<name>`) that defines extra listeners for TransportServers. Requires `-enable-custom-resources`. |
| `-enable-snippets` | `false` | `controller.enableSnippets` | Allow snippets in Ingresses (`server-snippet`, `configuration-snippet`, `stream-snippet`, `auth-snippet`, `nginx.org/*-snippets`), VirtualServers, VirtualServerRoutes and TransportServers. Without it snippet annotations are ignored with a warning. Snippets in the ConfigMap do not need it. |
| `-nginx-configmaps` | (none) | set by the chart (`controller.config`, `controller.customConfigMap`) | The [ConfigMap](configmap.md) (`<namespace>/<name>`) with the NGINX settings. The controller does not start if it is set but cannot be read. |

## ingress-nginx compatibility

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-ingress-nginx-defaults` | `false` | `controller.ingressNginxDefaults` (on with `controller.ingressNginxCompat`) | Use the defaults of ingress-nginx instead of those of NGINX Ingress Controller for everything neither the ConfigMap nor an annotation sets: timeouts, buffering, retries, upstream keepalive, the redirect code, HSTS, HTTP/2, TLS, the headers sent to the upstream, and WebSocket support in every Ingress location. It also makes the ConfigMap accept the keys of ingress-nginx (see [ConfigMap](configmap.md#ingress-nginx-keys)). |
| `-dynamic-upstreams` | `false` | `controller.dynamicUpstreams` (on with `controller.ingressNginxCompat`) | Update upstream servers, certificates, canaries and load-balancing rules through the API of the wxingress NGINX module instead of reloading NGINX. Required for canary Ingresses, ingress-nginx's balancers (`ewma`, `chash`, sticky sessions as ingress-nginx writes them) and the ingress-nginx request metrics. Ignored with `-nginx-plus`. |
| `-sync-rate-limit` | `0.3` | `controller.syncRateLimit` | With `-dynamic-upstreams`: the maximum number of NGINX reloads per second, as ingress-nginx's `--sync-rate-limit`. A change that comes sooner after a reload waits, and all changes that arrive meanwhile are applied in one reload. Changes that need no reload (upstream servers, canaries, certificates) do not wait. `0` disables the limit; a negative value is treated as `0`. |
| `-tcp-services-configmap` | (none) | `controller.tcpServices`, `controller.tcpServicesConfigMap` | A ConfigMap (`<namespace>/<name>`) of TCP services in ingress-nginx's format: `"<port>": "<namespace>/<service>:<port>[:PROXY][:PROXY]"`. NGINX listens on the port and passes connections to the Service; the first `PROXY` accepts the PROXY protocol from the client, the second sends it to the Service. Watched; with `-dynamic-upstreams` endpoint changes need no reload. |
| `-udp-services-configmap` | (none) | `controller.udpServices`, `controller.udpServicesConfigMap` | A ConfigMap (`<namespace>/<name>`) of UDP services: `"<port>": "<namespace>/<service>:<port>"`. |
| `-default-backend-service` | (none) | `controller.defaultBackendService` | A Service (`<namespace>/<name>`) that answers, as ingress-nginx's `--default-backend-service`, the requests for unknown hosts and paths, and the `custom-http-errors` of Ingresses without a `default-backend` annotation. Its first port is used; with `-dynamic-upstreams` endpoint changes need no reload. |

## Admission webhook

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-validating-webhook` | (none: no webhook) | `controller.admissionWebhooks.enabled`, `.port` | The address the admission webhook listens on with TLS, for example `:8443`, as ingress-nginx's `--validating-webhook`. A ValidatingWebhookConfiguration sends it the Ingresses being created or updated, and it refuses those that are invalid, that have the host and path of another Ingress, that carry a snippet annotation while snippets are disabled, or whose configuration fails `nginx -t`. |
| `-validating-webhook-certificate` | `/usr/local/certificates/cert` | `controller.admissionWebhooks.certificate` | The webhook's certificate (PEM). Read again when the file changes. |
| `-validating-webhook-key` | `/usr/local/certificates/key` | `controller.admissionWebhooks.key` | The key (PEM) of the webhook's certificate. |
| `-validating-webhook-nginx-test` | `true` | `controller.admissionWebhooks.nginxTest` | Run `nginx -t` on the configuration an Ingress would produce. Never done while `-enable-snippets` is on: whoever can reach the webhook could otherwise have a snippet executed by `nginx -t` (CVE-2025-1974, for which ingress-nginx dropped the test entirely). |

## Gateway API

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-enable-gateway-api` | `false` | `controller.gatewayAPI.enable` | Serve the Gateway API: the GatewayClass `-gateway-class` if it names `-gateway-controller-name`, its Gateways and their routes. Requires `-enable-custom-resources` and the Gateway API CRDs. |
| `-gateway-controller-name` | `wxingress.wizardtales.com/gateway-controller` | `controller.gatewayAPI.controllerName` | The `controllerName` of the GatewayClass to serve. |
| `-gateway-class` | `wxingress` | `controller.gatewayAPI.gatewayClass.name` | The name of the GatewayClass to serve. |
| `-default-gateway` | (none) | `controller.gatewayAPI.defaultGateway.name`, `.namespace` | The Gateway (`<namespace>/<name>`) that shares the address of the controller's Service (`-external-service`) and NGINX's default ports with Ingresses and VirtualServers, for a migration from Ingress. Its listeners may only be HTTP on 80 and HTTPS on 443. Sharing an address with other resources is a deliberate deviation from the Gateway API. |
| `-gateway-service-type` | `LoadBalancer` | `controller.gatewayAPI.service.type` | The type of the Services the controller creates for Gateways with their own address: `LoadBalancer`, `NodePort` or `ClusterIP`. |
| `-gateway-listener-ports` | `20000-29999` | `controller.gatewayAPI.listenerPorts` | The port range (`<first>-<last>`) NGINX listens on for the listeners of Gateways with their own address. |

## TLS

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-default-server-tls-secret` | (none) | `controller.defaultTLS.secret` (or `.cert`/`.key`) | A TLS Secret (`<namespace>/<name>`) for the default server, and for hosts without a valid certificate of their own. Without it the file `/etc/nginx/secrets/default` is used. If that file does not exist either, then with `-dynamic-upstreams` the controller generates ingress-nginx's self-signed "Kubernetes Ingress Controller Fake Certificate" at startup, and without it TLS handshakes to the default server are rejected. The controller does not start if the Secret is set but cannot be read. |
| `-wildcard-tls-secret` | (none) | `controller.wildcardTLS.secret` (or `.cert`/`.key`) | A TLS Secret (`<namespace>/<name>`) for every Ingress or VirtualServer host that enables TLS without naming a Secret. Without it, TLS handshakes for such hosts fail. |
| `-ssl-dynamic-reload` | `true` | `controller.enableSSLDynamicReload` | Load changed certificates without reloading NGINX. |
| `-enable-tls-passthrough` | `false` | `controller.enableTLSPassthrough` | Enable TLS passthrough (also used for ingress-nginx's `ssl-passthrough` annotation, the equivalent of its `--enable-ssl-passthrough`). Requires `-enable-custom-resources`. |
| `-tls-passthrough-port` | `443` | `controller.tlsPassthroughPort` | The port of TLS passthrough. |
| `-enable-cert-manager` | `false` | `controller.enableCertManager` | Let cert-manager issue certificates for VirtualServers. Requires `-enable-custom-resources`. |

## NGINX and listeners

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-default-http-listener-port` | `80` | `controller.defaultHTTPListenerPort` | The port of the HTTP `default_server` (and of Ingresses without a host). |
| `-default-https-listener-port` | `443` | `controller.defaultHTTPSListenerPort` | The port of the HTTPS `default_server` (and of Ingresses without a host). |
| `-disable-ipv6` | `false` | `controller.disableIPV6` | Do not open IPv6 listeners, for nodes without IPv6. |
| `-nginx-reload-timeout` | `60000` | `controller.nginxReloadTimeout` | How long, in milliseconds, the controller waits for a successful NGINX reload after a change or at startup. |
| `-enable-config-safety` | `false` | `controller.enableConfigSafety` | Validate the configuration before reloading NGINX. |
| `-enable-directive-autoadjust` | `false` | `controller.directiveAutoAdjust` | Adjust proxy buffer directives automatically so that they do not conflict. |
| `-nginx-debug` | `false` | `controller.nginxDebug` | Run the `nginx-debug` binary. Needs `error-log-level: debug` in the ConfigMap. |
| `-enable-topology-aware-routing` | `false` | `controller.enableTopologyAwareRouting` | Honour EndpointSlice topology hints, preferring endpoints in the same zone as the controller pod. Each replica then routes only to its own zone; spread replicas accordingly. The zone is read from the node at startup. |
| `-main-template-path` | built-in `nginx.tmpl` | (none) | Path to a replacement for the main NGINX configuration template. |
| `-ingress-template-path` | built-in `nginx.ingress.tmpl` | (none) | Path to a replacement for the Ingress template. |
| `-virtualserver-template-path` | built-in `nginx.virtualserver.tmpl` | (none) | Path to a replacement for the VirtualServer template. |
| `-transportserver-template-path` | built-in `nginx.transportserver.tmpl` | (none) | Path to a replacement for the TransportServer template. |

!!! warning "Custom templates"
    Replacing a template opts out of everything wxingress renders for ingress-nginx compatibility in that template.
    Prefer snippets or the ConfigMap; a replaced template must be kept in step with every wxingress release by hand.

## Status reporting and leader election

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-report-ingress-status` | `false` | `controller.reportIngressStatus.enable` (chart default `true`) | Write the controller's external address into the status of Ingresses. Needs `-external-service`, `-ingresslink` or the ConfigMap key `external-status-address`. |
| `-external-service` | (none) | `controller.reportIngressStatus.externalService` (default: the chart's Service) | The Service whose external address is reported in the status of Ingresses, VirtualServers and VirtualServerRoutes. With the Gateway API it is also the address of the default Gateway. |
| `-ingresslink` | (none) | `controller.reportIngressStatus.ingressLink` | The IngressLink resource (F5 BIG-IP) whose address is reported instead. Inherited from NIC, not tested in wxingress. |
| `-enable-leader-election` | `true` | `controller.reportIngressStatus.enableLeaderElection` | Elect one leader among the replicas. Only the leader writes status and manages shared objects (for example the Services of Gateways). The leader releases its lock when it stops, so another replica takes over within seconds. |
| `-leader-election-lock-name` | `nginx-ingress-leader-election` | `controller.reportIngressStatus.leaderElectionLockName` (default `<release>-leader-election`) | The name of the leader-election lock (a `coordination.k8s.io` Lease) in the controller's namespace. |
| `-enable-external-dns` | `false` | `controller.enableExternalDNS` | Create DNSEndpoint resources for external-dns from VirtualServers. Requires `-enable-custom-resources`. |

## Health, status and metrics

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-health-status` | `false` | `controller.healthStatus` | Add a location to the default server that answers 200 for external health checks. |
| `-health-status-uri` | `/nginx-health` | `controller.healthStatusURI` | The URI of that location. |
| `-ready-status` | `true` | `controller.readyStatus.enable` | Serve the readiness endpoint `/nginx-ready`, which succeeds once NGINX has loaded the full configuration after startup. |
| `-ready-status-port` | `8081` | `controller.readyStatus.port` | Its port (1024-65535). |
| `-nginx-status` | `true` | `controller.nginxStatus.enable` | Serve NGINX's `stub_status`. |
| `-nginx-status-port` | `8080` | `controller.nginxStatus.port` | Its port (1024-65535). |
| `-nginx-status-allow-cidrs` | `127.0.0.1,::1` | `controller.nginxStatus.allowCidrs` (chart default `127.0.0.1`) | Comma-separated addresses and CIDRs allowed to read `stub_status`. |
| `-enable-prometheus-metrics` | `false` | `prometheus.create` (chart default `true`) | Export metrics in the Prometheus format: the controller's own and, with `-dynamic-upstreams`, the request metrics of ingress-nginx under its names and labels. |
| `-prometheus-metrics-listen-port` | `9113` | `prometheus.port` | The metrics port (1024-65535). ingress-nginx used 10254; set this if your scrape configuration expects it. |
| `-prometheus-tls-secret` | (none) | `prometheus.secret` | A TLS Secret (`<namespace>/<name>`) to serve the metrics over HTTPS. |
| `-enable-latency-metrics` | `false` | `controller.enableLatencyMetrics` | Collect upstream latency metrics. Requires `-enable-prometheus-metrics`, else ignored with a warning. |
| `-latency-metrics-buckets` | built-in set | `controller.latencyMetricsBuckets` | Comma-separated, strictly ascending bucket bounds in milliseconds for the latency histogram, for example `5,10,25,50,100,250,500,1000`. Requires `-enable-latency-metrics`; an invalid list falls back to the default buckets. |

## OIDC

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-enable-oidc` | `false` | `controller.enableOIDC` | Enable OIDC Policies. |
| `-oidc-revocations-configmap` | (none) | set by the chart with `controller.enableOIDC` and dynamic upstreams | With `-enable-oidc` and `-dynamic-upstreams`: a ConfigMap (`<namespace>/<name>`) whose `revocations` end sessions of the wxingress module before their ID tokens expire. Each replica writes the logouts it handled into it, so a logout reaches all replicas without a reload. |

## Logging and miscellaneous

| Flag | Default | Helm value | Description |
|---|---|---|---|
| `-log-level` | `info` | `controller.logLevel` | `trace`, `debug`, `info`, `warning`, `error` or `fatal`. An invalid value falls back to `info` with a warning. |
| `-log-format` | `glog` | `controller.logFormat` | `glog`, `text` or `json` (also accepted: `json-unix`, `json-unix-ms`, `text-unix`, `text-unix-ms`). An invalid value falls back to `glog` with a warning. |
| `-version` | | | Print the version, git commit and build date, and exit. |
| `-proxy` | (none) | | Connect to the Kubernetes API through `kubectl proxy` at this URL. For development only: NGINX is not started and no configuration is written. |
| `-enable-telemetry-reporting` | `false` | `controller.telemetryReporting.enable` | Ignored: wxingress has no telemetry. Kept so that existing arguments keep working. |

## Inherited from NGINX Ingress Controller, not supported in wxingress

wxingress is built on NGINX Ingress Controller and keeps its flags for NGINX Plus, F5 WAF (App Protect), App Protect
DoS and NGINX Agent, so that the code base stays close to upstream. wxingress ships an open-source NGINX image;
these flags are neither supported nor tested in wxingress. Their meaning is that of the upstream
[NGINX Ingress Controller command-line arguments](https://docs.nginx.com/nginx-ingress-controller/configuration/global-configuration/command-line-arguments/).

| Flag | Default | Notes |
|---|---|---|
| `-nginx-plus` | `false` | NGINX Plus. Turns `-dynamic-upstreams` off. |
| `-mgmt-configmap` | (none) | The NGINX Plus `mgmt` ConfigMap. Ignored with a warning without `-nginx-plus`. |
| `-enable-app-protect` | `false` | App Protect WAF. Requires `-nginx-plus`. |
| `-app-protect-log-level` | `fatal` | |
| `-app-protect-enforcer-address` | `127.0.0.1:50000` | App Protect v5 enforcer. |
| `-enable-app-protect-ip-intelligence` | `false` | |
| `-plm-storage-url`, `-plm-storage-credentials-secret`, `-plm-storage-ca-secret`, `-plm-storage-client-ssl-secret`, `-plm-storage-insecure-skip-verify` | (none), `false` | F5 WAF Policy Controller storage. |
| `-enable-app-protect-dos` | `false` | App Protect DoS. Requires `-nginx-plus`. |
| `-app-protect-dos-debug`, `-app-protect-dos-max-daemons`, `-app-protect-dos-max-workers`, `-app-protect-dos-memory` | `false`, `0`, `0`, `0` | |
| `-enable-service-insight`, `-service-insight-listen-port`, `-service-insight-tls-secret` | `false`, `9114`, (none) | NGINX Plus only; ignored with a warning otherwise. |
| `-weight-changes-dynamic-reload` | `false` | NGINX Plus only; ignored with a warning otherwise. |
| `-agent`, `-agent-instance-group` | `false`, `nginx-ingress-controller` | NGINX Agent. |
