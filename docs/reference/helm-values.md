# Helm values

The `wxingress` Helm chart deploys the controller, its Service, IngressClass, RBAC, ConfigMap and, optionally, the
admission webhook, the GatewayClass and the Gateway API CRDs. This page lists the values that matter for running
wxingress, grouped by topic. The complete list, with a comment for each value, is the chart's `values.yaml`
(`helm show values oci://ghcr.io/wx-one/charts/wxingress`); `values.schema.json` validates them on every install
and upgrade. Most values become a [command-line flag](cli-flags.md) of the controller; NGINX settings go into the
[ConfigMap](configmap.md) through `controller.config.entries`.

## Installing

```sh
helm install wxingress oci://ghcr.io/wx-one/charts/wxingress \
  --namespace wxingress --create-namespace
```

To run in the place of ingress-nginx, add `--set controller.ingressNginxCompat=true` (see
[IngressClass and ingress-nginx compatibility](#ingressclass-and-ingress-nginx-compatibility) below).

The chart installs the custom resource definitions of wxingress (VirtualServer, TransportServer, Policy, ...) from its
`crds/` directory on the first install. Helm does not upgrade or delete CRDs: apply them from the release when you
upgrade. The Gateway API CRDs are optional, see [Gateway API](#gateway-api).

## Controller workload

| Value | Default | Description |
|---|---|---|
| `controller.kind` | `deployment` | `deployment`, `daemonset` or `statefulset`. |
| `controller.replicaCount` | `1` | Replicas of the Deployment or StatefulSet. |
| `controller.name` | `controller` | Name of the workload. |
| `controller.strategy` | `{}` | The update strategy: `strategy` of a Deployment, `updateStrategy` of a DaemonSet or StatefulSet, as Kubernetes YAML. |
| `controller.resources` | requests `100m` CPU, `128Mi` memory | Resources of the controller container. |
| `controller.tolerations` | `[]` | Tolerations of the pods. |
| `controller.affinity` | `{}` | Affinity of the pods. |
| `controller.nodeSelector` | (unset) | Node selector of the pods. |
| `controller.topologySpreadConstraints` | (unset) | Topology spread constraints of the pods. |
| `controller.priorityClassName` | (unset) | PriorityClass of the pods. |
| `controller.terminationGracePeriodSeconds` | `30` | Grace period of the pods. |
| `controller.minReadySeconds` | `0` | Minimum ready time before a new pod counts as available. |
| `controller.lifecycle` | `{}` | Lifecycle hooks of the controller container. With `controller.ingressNginxCompat` and no lifecycle of your own the chart adds a `preStop` of `sleep 5`. |
| `controller.podDisruptionBudget.enabled` | `false` | Create a PodDisruptionBudget (`minAvailable` or `maxUnavailable`). |
| `controller.autoscaling.enabled` | `false` | Create a HorizontalPodAutoscaler (`minReplicas` 1, `maxReplicas` 3, CPU and memory targets 50 %). |
| `controller.pod.annotations`, `controller.pod.extraLabels` | `{}` | Annotations and labels of the pods. |
| `controller.annotations` | `{}` | Annotations of the workload. |
| `commonLabels` | `{}` | Labels for every object of the chart (not CRDs, not selector labels). |
| `controller.env`, `controller.volumes`, `controller.volumeMounts`, `controller.initContainers`, `controller.extraContainers` | `[]` | Extra environment, volumes and containers. |
| `controller.podSecurityContext` | `seccompProfile: RuntimeDefault` | Pod security context. |
| `controller.securityContext` | `{}` | Container security context. |
| `controller.readOnlyRootFilesystem` | `false` | Read-only root file system with writable volumes for NGINX's temporary data. Not used when `controller.securityContext` is set. |
| `controller.readyStatus.enable`, `.port`, `.initialDelaySeconds` | `true`, `8081`, `0` | The readiness probe on `/nginx-ready`. |
| `controller.startupStatus.enable` | `false` | A startup probe (`port` and `path` required when enabled). |
| `controller.networkPolicy.enabled` | `false` | Create a NetworkPolicy for the pods with `ingress` and `egress` rules. |
| `rbac.create`, `rbac.clusterrole.create` | `true`, `true` | Create the RBAC objects. |
| `controller.serviceAccount.name`, `.annotations`, `.imagePullSecretName`, `.imagePullSecretsNames` | generated, `{}`, `""`, `[]` | The ServiceAccount of the pods and its pull Secrets. |

## Image

| Value | Default | Description |
|---|---|---|
| `controller.image.repository` | `ghcr.io/wx-one/wxingress` | The image repository. |
| `controller.image.tag` | the chart's `appVersion` | The image tag. |
| `controller.image.digest` | (unset) | An image digest; wins over the tag. |
| `controller.image.pullPolicy` | `IfNotPresent` | The pull policy. |

## Network: Service, hostPort, host network

| Value | Default | Description |
|---|---|---|
| `controller.service.create` | `true` | Create a Service for the controller pods. |
| `controller.service.type` | `LoadBalancer` | `LoadBalancer`, `NodePort` or `ClusterIP`. |
| `controller.service.externalTrafficPolicy` | `Local` | `Local` preserves the client's source address. |
| `controller.service.annotations`, `.extraLabels` | `{}` | Annotations (for example of your cloud load balancer) and labels of the Service. |
| `controller.service.loadBalancerIP`, `.loadBalancerSourceRanges`, `.externalIPs`, `.clusterIP` | empty | The usual Service fields. |
| `controller.service.ipFamilyPolicy`, `.ipFamilies` | (unset) | Dual-stack settings. |
| `controller.service.httpPort`, `.httpsPort` | `80`, `443` | `enable`, `port`, `nodePort`, `targetPort`, `name` of the two ports. |
| `controller.service.customPorts` | `[]` | More Service ports. |
| `controller.service.additionalServices` | `{}` | More Services for the same pods, each with its own type, annotations and ports (status is reported from the main Service only). |
| `controller.service.sessionAffinity.enable` | `false` | `ClientIP` session affinity of the Service. |
| `controller.hostPort.enable` | `false` | Open hostPorts on the nodes, as ingress-nginx's DaemonSet setups do: `controller.hostPort.http` (`80`) and `.https` (`443`), and the ports of `controller.tcpServices`/`udpServices`. Usually combined with `controller.kind: daemonset` and `controller.service.create: false` or `type: ClusterIP`. |
| `controller.containerPort.http`, `.https` | `80`, `443` | The container ports. |
| `controller.customPorts` | `[]` | More container ports. |
| `controller.hostNetwork` | `false` | Use the node's network namespace. Combine with `controller.dnsPolicy: ClusterFirstWithHostNet`. |
| `controller.dnsPolicy` | `ClusterFirst` | DNS policy of the pods. |
| `controller.defaultHTTPListenerPort`, `controller.defaultHTTPSListenerPort` | `80`, `443` | The ports of NGINX's default server. |
| `controller.disableIPV6` | `false` | No IPv6 listeners. |

A DaemonSet with hostPorts on every node, the usual bare-metal setup of ingress-nginx:

```yaml
controller:
  kind: daemonset
  hostPort:
    enable: true
  service:
    type: ClusterIP
```

## IngressClass and ingress-nginx compatibility

| Value | Default | Description |
|---|---|---|
| `controller.ingressClass.name` | `nginx` | The class the controller serves (`-ingress-class`). |
| `controller.ingressClass.create` | `true` | Create the IngressClass. Set `false` to serve an existing class (for example ingress-nginx's) without taking it over. Do not change it between upgrades: Helm would delete the IngressClass. |
| `controller.ingressClass.setAsDefaultIngress` | `true` | Mark the IngressClass as the cluster's default: new Ingresses without a class get it, existing ones without a class are served. Requires `create`. |
| `controller.ingressClass.controllerValue` | `nginx.wx1.eu/ingress-controller` | The `spec.controller` of the IngressClass the chart creates. `spec.controller` is immutable: when upgrading a release that used `nginx.org/ingress-controller`, keep that value or delete the IngressClass first. |
| `controller.ingressClass.controllers` | `[]` | More IngressClass controller values whose classes are served (`-ingress-class-controllers`), for example `k8s.io/ingress-nginx` to serve ingress-nginx's classes. |
| `controller.watchIngressWithoutClass` | `true` | Serve Ingresses without a class (`-watch-ingress-without-class`). |
| `controller.ingressNginxCompat` | `false` | The profile for running in the place of ingress-nginx: turns on `ingressNginxDefaults`, `dynamicUpstreams` and `allowEmptyIngressHost`, and adds a `preStop` sleep (unless `controller.lifecycle` is set) so that the load balancer stops sending before NGINX stops. |
| `controller.ingressNginxDefaults` | `false` | Use ingress-nginx's defaults and accept its ConfigMap keys (`-ingress-nginx-defaults`). |
| `controller.dynamicUpstreams` | `false` | Update upstreams, certificates and canaries through the wxingress module instead of reloading NGINX (`-dynamic-upstreams`). Needs the wxingress image. |
| `controller.syncRateLimit` | `0.3` | With dynamic upstreams, the maximum number of reloads per second (`-sync-rate-limit`), as in ingress-nginx. `0` for no limit. |
| `controller.allowEmptyIngressHost` | `false` | Accept Ingresses without a host (`-allow-empty-ingress-host`). |
| `controller.tcpServices`, `controller.udpServices` | `{}` | ingress-nginx's `tcp` and `udp` values: `"<port>": "<namespace>/<service>:<port>[:PROXY][:PROXY]"`. The chart writes them into a ConfigMap, opens the ports in the pods (and on the nodes with `controller.hostPort.enable`) and adds them to the Service. |
| `controller.tcpServicesConfigMap`, `controller.udpServicesConfigMap` | `""` | An existing ConfigMap of that format (`<namespace>/<name>`) instead; the chart opens no ports for it (use `controller.customPorts` and `controller.service.customPorts`). |
| `controller.defaultBackendService` | `""` | ingress-nginx's `--default-backend-service`: a Service (`<namespace>/<name>`) for unknown hosts and paths and for `custom-http-errors`. |

!!! warning "Status while both controllers run"
    While wxingress and ingress-nginx serve the same Ingresses, only one of them should write their status. Turn
    `controller.reportIngressStatus.enable` off in wxingress until it has taken over.

A typical migration values file:

```yaml
controller:
  image:
    repository: ghcr.io/wx-one/wxingress
  ingressNginxCompat: true
  ingressClass:
    create: false          # ingress-nginx still owns the IngressClass "nginx"
    controllers:
      - k8s.io/ingress-nginx
  reportIngressStatus:
    enable: false          # until ingress-nginx is gone
```

## ConfigMap and snippets

| Value | Default | Description |
|---|---|---|
| `controller.config.entries` | `{}` | The keys of the controller's [ConfigMap](configmap.md). With `ingressNginxDefaults` (or `ingressNginxCompat`) the keys of ingress-nginx's `controller.config` are accepted as well. |
| `controller.config.name` | generated | The name of the ConfigMap. |
| `controller.config.annotations` | `{}` | Annotations of the ConfigMap. |
| `controller.customConfigMap` | `""` | Use an existing ConfigMap instead of the one the chart creates. |
| `controller.enableSnippets` | `false` | Allow snippet annotations and snippets in custom resources (`-enable-snippets`). |

## TLS

| Value | Default | Description |
|---|---|---|
| `controller.defaultTLS.secret` | `""` | A TLS Secret (`<namespace>/<name>`) for the default server and for hosts without a valid certificate. |
| `controller.defaultTLS.cert`, `.key` | `""` | Or a base64-encoded certificate and key, which the chart puts into a Secret. |
| `controller.wildcardTLS.secret`, `.cert`, `.key` | `""` | A certificate for hosts that enable TLS without naming a Secret. |
| `controller.enableSSLDynamicReload` | `true` | Load changed certificates without a reload. |
| `controller.enableTLSPassthrough`, `controller.tlsPassthroughPort` | `false`, `443` | TLS passthrough, used by ingress-nginx's `ssl-passthrough` annotation. |
| `controller.enableCertManager` | `false` | cert-manager integration for VirtualServers. |

## Admission webhook

The admission webhook mirrors ingress-nginx's `controller.admissionWebhooks`. A ValidatingWebhookConfiguration sends
every Ingress being created or updated to the controller pods. The pods refuse an Ingress that is invalid, that
reuses the host and path of another Ingress, that has a snippet annotation while snippets are disabled, or whose
configuration fails `nginx -t`.

| Value | Default | Description |
|---|---|---|
| `controller.admissionWebhooks.enabled` | `false` | Deploy the webhook. |
| `controller.admissionWebhooks.failurePolicy` | `Fail` | With `Fail`, no Ingress can be created or changed while no controller pod is ready. |
| `controller.admissionWebhooks.timeoutSeconds` | `10` | Timeout of a review. |
| `controller.admissionWebhooks.port` | `8443` | The port the pods serve the webhook on. |
| `controller.admissionWebhooks.nginxTest` | `true` | Run `nginx -t` on the configuration an Ingress would produce. Never done while `controller.enableSnippets` is on. |
| `controller.admissionWebhooks.namespaceSelector`, `.objectSelector` | `{}` | Which Ingresses are reviewed. |
| `controller.admissionWebhooks.service` | `ClusterIP`, port `443` | The webhook's Service. |
| `controller.admissionWebhooks.patch.enabled` | `true` | Create a self-signed certificate with the kube-webhook-certgen jobs, as ingress-nginx's chart does (`createSecretJob`, `patchWebhookJob`, `patch.image`). |
| `controller.admissionWebhooks.certManager.enabled` | `false` | Use cert-manager for the certificate instead (a self-signed root CA and Issuer, or `issuerRef`). |

!!! tip
    Enable the webhook once the controller pods run. With `failurePolicy: Fail` and no ready pod, Ingress changes are
    refused.

## Gateway API

| Value | Default | Description |
|---|---|---|
| `controller.gatewayAPI.enable` | `false` | Serve the Gateway API (`-enable-gateway-api`). Requires `controller.enableCustomResources` and the Gateway API CRDs. |
| `controller.gatewayAPI.installCRDs` | `false` | Install the standard-channel Gateway API CRDs of the version wxingress is built with. They are kept on uninstall. CRDs that already exist and do not belong to this release are left alone. |
| `controller.gatewayAPI.controllerName` | `wxingress.wizardtales.com/gateway-controller` | The `controllerName` of the GatewayClass this release serves. |
| `controller.gatewayAPI.gatewayClass.name` | `wxingress` | The GatewayClass this release serves. |
| `controller.gatewayAPI.gatewayClass.create` | `true` | Create the GatewayClass (as a post-install and post-upgrade hook, after the CRDs). |
| `controller.gatewayAPI.defaultGateway.name`, `.namespace` | `""`, release namespace | The default Gateway, which shares the controller Service's address and ports 80/443 with Ingresses (a deliberate deviation from the Gateway API, for migrations). The Gateway itself is not part of the release. |
| `controller.gatewayAPI.service.type` | `LoadBalancer` | The type of the Services the controller creates for every other Gateway. |
| `controller.gatewayAPI.listenerPorts` | `20000-29999` | The NGINX ports for the listeners of those Gateways. |

## Status and leader election

| Value | Default | Description |
|---|---|---|
| `controller.reportIngressStatus.enable` | `true` | Write the controller's address into the status of Ingresses. |
| `controller.reportIngressStatus.externalService` | the chart's Service | The Service whose address is reported. |
| `controller.reportIngressStatus.ingressLink` | `""` | An F5 IngressLink instead (inherited, untested). |
| `controller.reportIngressStatus.enableLeaderElection` | `true` | Leader election among the replicas; only the leader writes status. |
| `controller.reportIngressStatus.leaderElectionLockName` | `<release>-leader-election` | The name of the Lease. The chart creates it and keeps it on uninstall, so a reinstall can take it over. |
| `controller.reportIngressStatus.annotations` | `{}` | Annotations of the Lease. |

## Metrics

| Value | Default | Description |
|---|---|---|
| `prometheus.create` | `true` | Export Prometheus metrics (`-enable-prometheus-metrics`). With dynamic upstreams this includes ingress-nginx's request metrics under their names, so its dashboards work. |
| `prometheus.port` | `9113` | The metrics port. ingress-nginx used `10254`. |
| `prometheus.scheme`, `prometheus.secret` | `http`, `""` | Serve metrics over HTTPS with a TLS Secret (`<namespace>/<name>`). |
| `prometheus.service.create` | `false` | Create a ClusterIP Service for the metrics, with the labels in `prometheus.service.labels`. |
| `prometheus.serviceMonitor.create` | `false` | Create a ServiceMonitor (Prometheus Operator). It selects the metrics Service, so also set `prometheus.service.create`. |
| `prometheus.serviceMonitor.labels` | `{}` | Labels of the ServiceMonitor, for example the one your Prometheus selects. |
| `prometheus.serviceMonitor.jobLabel` | `""` | The label of the metrics Service whose value becomes the job name (ingress-nginx used `component`). |
| `prometheus.serviceMonitor.selectorMatchLabels`, `.endpoints` | `service: wxingress-prometheus-service`, port `prometheus` | Selector and endpoints of the ServiceMonitor. |
| `controller.enableLatencyMetrics`, `controller.latencyMetricsBuckets` | `false`, `""` | Upstream latency histogram and its buckets. |
| `controller.nginxStatus.enable`, `.port`, `.allowCidrs` | `true`, `8080`, `127.0.0.1` | NGINX `stub_status`. |

## Other controller settings

| Value | Default | Description |
|---|---|---|
| `controller.logLevel` | `info` | `trace`, `debug`, `info`, `warning`, `error`, `fatal`. |
| `controller.logFormat` | `glog` | `glog`, `json` or `text`, or `json-unix`, `json-unix-ms`, `text-unix`, `text-unix-ms` (the time as Unix seconds or milliseconds). |
| `controller.watchNamespace`, `controller.watchNamespaceLabel`, `controller.watchSecretNamespace` | `""` | Restrict the watched namespaces. |
| `controller.enableCustomResources` | `true` | Serve VirtualServer, TransportServer, Policy and the other custom resources. |
| `controller.globalConfiguration.create`, `.spec`, `.customName` | `false` | A GlobalConfiguration with extra listeners for TransportServers. |
| `controller.enableOIDC` | `false` | OIDC Policies. With dynamic upstreams the chart also creates the key the wxingress module seals sessions with (a Secret kept across upgrades) and passes `-oidc-revocations-configmap`. |
| `controller.enableExternalDNS` | `false` | external-dns integration for VirtualServers. |
| `controller.enableTopologyAwareRouting` | `false` | Prefer endpoints in the controller's zone. |
| `controller.healthStatus`, `controller.healthStatusURI` | `false`, `/nginx-health` | A health-check location in the default server. |
| `controller.nginxReloadTimeout` | `60000` | Reload timeout in milliseconds. |
| `controller.enableConfigSafety` | `false` | Validate the configuration before reloading. |
| `controller.directiveAutoAdjust` | `false` | Adjust conflicting proxy buffer settings automatically. |
| `controller.nginxDebug` | `false` | Run `nginx-debug`. |

## Inherited values that do not apply to wxingress

The chart keeps the values of NGINX Ingress Controller for NGINX Plus and F5 products so that it stays close to
upstream: `controller.nginxplus`, `controller.mgmt`, `controller.appprotect`, `controller.appprotectdos`,
`controller.enableWeightChangesDynamicReload`, `serviceInsight` and `nginxAgent`. They are not supported or tested in
wxingress, which runs open-source NGINX. `controller.telemetryReporting.enable` has no effect: wxingress has no
telemetry.
