package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"math"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/gateway"
	"github.com/wx-one/wxingress/internal/helpers"
	"github.com/wx-one/wxingress/internal/k8s"
	internalValidation "github.com/wx-one/wxingress/internal/validation"
	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"

	nl "github.com/wx-one/wxingress/internal/logger"
)

const (
	dynamicSSLReloadParam         = "ssl-dynamic-reload"
	dynamicWeightChangesParam     = "weight-changes-dynamic-reload"
	appProtectLogLevelDefault     = "fatal"
	appProtectEnforcerAddrDefault = "127.0.0.1:50000"
	logLevelDefault               = "info"
	logFormatDefault              = "glog"
)

var (
	healthStatus = flag.Bool("health-status", false,
		`Add a location based on the value of health-status-uri to the default server. The location responds with the 200 status code for any request.
	Useful for external health-checking of the Ingress Controller`)

	healthStatusURI = flag.String("health-status-uri", "/nginx-health",
		`Sets the URI of health status location in the default server. Requires -health-status`)

	proxyURL = flag.String("proxy", "",
		`Use a proxy server to connect to Kubernetes API started by "kubectl proxy" command. For testing purposes only.
	The Ingress Controller does not start NGINX and does not write any generated NGINX configuration files to disk`)

	watchNamespace = flag.String("watch-namespace", api_v1.NamespaceAll,
		`Comma separated list of namespaces the Ingress Controller should watch for resources. By default the Ingress Controller watches all namespaces. Mutually exclusive with "watch-namespace-label".`)

	watchNamespaces []string

	watchSecretNamespace = flag.String("watch-secret-namespace", "",
		`Comma separated list of namespaces the Ingress Controller should watch for secrets. If this arg is not configured, the Ingress Controller watches the same namespaces for all resources. See "watch-namespace" and "watch-namespace-label". `)

	watchSecretNamespaces []string

	watchNamespaceLabel = flag.String("watch-namespace-label", "",
		`Configures the Ingress Controller to watch only those namespaces with label foo=bar. By default the Ingress Controller watches all namespaces. Mutually exclusive with "watch-namespace". `)

	nginxConfigMaps = flag.String("nginx-configmaps", "",
		`A ConfigMap resource for customizing NGINX configuration. If a ConfigMap is set,
	but the Ingress Controller is not able to fetch it from Kubernetes API, the Ingress Controller will fail to start.
	Format: <namespace>/<name>`)

	mgmtConfigMap = flag.String("mgmt-configmap", "",
		`A ConfigMap resource for customizing NGINX configuration. If a ConfigMap is set,
	but the Ingress Controller is not able to fetch it from Kubernetes API, the Ingress Controller will fail to start.
	Format: <namespace>/<name>`)

	nginxPlus = flag.Bool("nginx-plus", false, "Enable support for NGINX Plus")

	dynamicUpstreams = flag.Bool("dynamic-upstreams", false,
		`Update the servers of upstreams through the upstream API of the wxingress module in NGINX OSS instead of reloading NGINX. Ignored with -nginx-plus, which has its own API.`)

	syncRateLimit = flag.Float64("sync-rate-limit", 0.3,
		`With -dynamic-upstreams, the most reloads of NGINX a second, as ingress-nginx's --sync-rate-limit: a change that comes sooner after a reload waits, and every change that comes meanwhile is applied with it, in one reload. What needs no reload - the servers of upstreams, canaries, certificates - does not wait. 0 for no limit.`)

	validatingWebhook = flag.String("validating-webhook", "",
		`The address the admission webhook listens on, with TLS, as ingress-nginx's --validating-webhook (":8443"): a ValidatingWebhookConfiguration sends it the Ingresses to be created or updated, and it refuses those that would be invalid, that have the host and path of another Ingress, or whose configuration would not pass nginx -t. Empty: no webhook.`)

	validatingWebhookCertificate = flag.String("validating-webhook-certificate", "/usr/local/certificates/cert",
		`The certificate (PEM) of the admission webhook, read again when it changes (ingress-nginx's --validating-webhook-certificate).`)

	validatingWebhookKey = flag.String("validating-webhook-key", "/usr/local/certificates/key",
		`The key (PEM) of the certificate of the admission webhook (ingress-nginx's --validating-webhook-key).`)

	validatingWebhookNginxTest = flag.Bool("validating-webhook-nginx-test", true,
		`Run nginx -t of the configuration an Ingress the admission webhook checks would make. Never with -enable-snippets: whoever reaches the webhook could have a snippet run by nginx -t (CVE-2025-1974, for which ingress-nginx stopped testing altogether).`)

	ingressNginxDefaults = flag.Bool("ingress-nginx-defaults", false,
		`Use the defaults of ingress-nginx instead of those of this controller for what neither the ConfigMap nor an annotation sets: timeouts, buffering, retries, keepalive, redirects, HSTS, HTTP/2, TLS, the headers to the upstream and WebSocket in every Ingress location. For clusters moving from ingress-nginx.`)

	appProtect = flag.Bool("enable-app-protect", false, "Enable support for NGINX App Protect. Requires -nginx-plus.")

	appProtectLogLevel = flag.String("app-protect-log-level", appProtectLogLevelDefault,
		`Sets log level for App Protect. Allowed values: fatal, error, warn, info, debug, trace. Requires -nginx-plus and -enable-app-protect.`)

	appProtectDos = flag.Bool("enable-app-protect-dos", false, "Enable support for NGINX App Protect dos. Requires -nginx-plus.")

	appProtectDosDebug = flag.Bool("app-protect-dos-debug", false, "Enable debugging for App Protect Dos. Requires -nginx-plus and -enable-app-protect-dos.")

	appProtectDosMaxDaemons = flag.Int("app-protect-dos-max-daemons", 0, "Max number of ADMD instances. Requires -nginx-plus and -enable-app-protect-dos.")
	appProtectDosMaxWorkers = flag.Int("app-protect-dos-max-workers", 0, "Max number of nginx processes to support. Requires -nginx-plus and -enable-app-protect-dos.")
	appProtectDosMemory     = flag.Int("app-protect-dos-memory", 0, "RAM memory size to consume in MB. Requires -nginx-plus and -enable-app-protect-dos.")

	appProtectEnforcerAddress = flag.String("app-protect-enforcer-address", appProtectEnforcerAddrDefault,
		`Sets address for App Protect v5 Enforcer. Requires -nginx-plus and -enable-app-protect.`)

	appProtectIPIntelligence = flag.Bool("enable-app-protect-ip-intelligence", false, "Enable App Protect IP Intelligence. Requires -nginx-plus and -enable-app-protect.")

	plmStorageURL = flag.String("plm-storage-url", "",
		`SeaweedFS S3 endpoint URL for the F5 WAF Policy Controller (PLM). When non-empty,
enables the PLM integration: NIC watches the appprotect.f5.com/v1 CRDs installed by the PLM
Helm chart instead of the legacy v1beta1 CRDs. Empty (default) disables PLM.
Must be http:// or https://. Requires -nginx-plus and -enable-app-protect.`)

	plmStorageCredentialsSecret = flag.String("plm-storage-credentials-secret", "",
		`Kubernetes Secret holding the S3 admin key for the PLM SeaweedFS filer under the
seaweedfs_admin_secret key. Format: namespace/name. Requires -plm-storage-url.`)

	plmStorageCASecret = flag.String("plm-storage-ca-secret", "",
		`Kubernetes Secret containing a ca.crt for verifying the PLM SeaweedFS server
certificate. Format: namespace/name. Optional. Requires -plm-storage-url.`)

	plmStorageClientSSLSecret = flag.String("plm-storage-client-ssl-secret", "",
		`Kubernetes Secret containing tls.crt and tls.key for mTLS to the PLM SeaweedFS
filer. Format: namespace/name. Optional. Requires -plm-storage-url.`)

	plmStorageInsecureSkipVerify = flag.Bool("plm-storage-insecure-skip-verify", false,
		`Disable TLS verification of the PLM SeaweedFS server certificate. For dev/test only.
NIC prints a startup warning when set. Requires -plm-storage-url.`)

	agent              = flag.Bool("agent", false, "Enable NGINX Agent")
	agentInstanceGroup = flag.String("agent-instance-group", "nginx-ingress-controller", "Grouping used to associate NGINX Ingress Controller instances")

	ingressClass = flag.String("ingress-class", "nginx",
		`A class of the Ingress Controller.

	An IngressClass resource with the name equal to the class must be deployed. Otherwise, the Ingress Controller will fail to start.
	The Ingress Controller only processes resources that belong to its class - i.e. have the "ingressClassName" field resource equal to the class.

	The Ingress Controller processes all the VirtualServer/VirtualServerRoute/TransportServer resources that do not have the "ingressClassName" field for all versions of kubernetes.`)

	defaultServerSecret = flag.String("default-server-tls-secret", "",
		`A Secret with a TLS certificate and key for TLS termination of the default server. Format: <namespace>/<name>.
	If not set, than the certificate and key in the file "/etc/nginx/secrets/default" are used.
	If "/etc/nginx/secrets/default" doesn't exist, the Ingress Controller will configure NGINX to reject TLS connections to the default server.
	If a secret is set, but the Ingress Controller is not able to fetch it from Kubernetes API or it is not set and the Ingress Controller
	fails to read the file "/etc/nginx/secrets/default", the Ingress Controller will fail to start.`)

	versionFlag = flag.Bool("version", false, "Print the version, git-commit hash and build date and exit")

	mainTemplatePath = flag.String("main-template-path", "",
		`Path to the main NGINX configuration template. (default for NGINX "nginx.tmpl"; default for NGINX Plus "nginx-plus.tmpl")`)

	ingressTemplatePath = flag.String("ingress-template-path", "",
		`Path to the ingress NGINX configuration template for an ingress resource.
	(default for NGINX "nginx.ingress.tmpl"; default for NGINX Plus "nginx-plus.ingress.tmpl")`)

	virtualServerTemplatePath = flag.String("virtualserver-template-path", "",
		`Path to the VirtualServer NGINX configuration template for a VirtualServer resource.
	(default for NGINX "nginx.virtualserver.tmpl"; default for NGINX Plus "nginx-plus.virtualserver.tmpl")`)

	transportServerTemplatePath = flag.String("transportserver-template-path", "",
		`Path to the TransportServer NGINX configuration template for a TransportServer resource.
	(default for NGINX "nginx.transportserver.tmpl"; default for NGINX Plus "nginx-plus.transportserver.tmpl")`)

	externalService = flag.String("external-service", "",
		`Specifies the name of the service with the type LoadBalancer through which the Ingress Controller pods are exposed externally.
	The external address of the service is used when reporting the status of Ingress, VirtualServer and VirtualServerRoute resources. For Ingress resources only: Requires -report-ingress-status.`)

	ingressLink = flag.String("ingresslink", "",
		`Specifies the name of the IngressLink resource, which exposes the Ingress Controller pods via a BIG-IP system.
	The IP of the BIG-IP system is used when reporting the status of Ingress, VirtualServer and VirtualServerRoute resources. For Ingress resources only: Requires -report-ingress-status.`)

	reportIngressStatus = flag.Bool("report-ingress-status", false,
		"Updates the address field in the status of Ingress resources. Requires the -external-service or -ingresslink flag, or the 'external-status-address' key in the ConfigMap.")

	leaderElectionEnabled = flag.Bool("enable-leader-election", true,
		"Enable Leader election to avoid multiple replicas of the controller reporting the status of Ingress, VirtualServer and VirtualServerRoute resources -- only one replica will report status (default true). See -report-ingress-status flag.")

	leaderElectionLockName = flag.String("leader-election-lock-name", "nginx-ingress-leader-election",
		`Specifies the name of the ConfigMap, within the same namespace as the controller, used as the lock for leader election. Requires -enable-leader-election.`)

	nginxStatusAllowCIDRs = flag.String("nginx-status-allow-cidrs", "127.0.0.1,::1", `Add IP/CIDR blocks to the allow list for NGINX stub_status or the NGINX Plus API. Separate multiple IP/CIDR by commas.`)

	allowedCIDRs []string

	nginxStatusPort = flag.Int("nginx-status-port", 8080,
		"Set the port where the NGINX stub_status or the NGINX Plus API is exposed. [1024 - 65535]")

	nginxStatus = flag.Bool("nginx-status", true,
		"Enable the NGINX stub_status, or the NGINX Plus API.")

	nginxDebug = flag.Bool("nginx-debug", false,
		"Enable debugging for NGINX. Uses the nginx-debug binary. Requires 'error-log-level: debug' in the ConfigMap.")

	enableConfigSafety = flag.Bool("enable-config-safety", false,
		"Enable config validation prior to reloading NGINX.")

	nginxReloadTimeout = flag.Int("nginx-reload-timeout", 60000,
		`The timeout in milliseconds which the Ingress Controller will wait for a successful NGINX reload after a change or at the initial start. (default 60000)`)

	wildcardTLSSecret = flag.String("wildcard-tls-secret", "",
		`A Secret with a TLS certificate and key for TLS termination of every Ingress/VirtualServer host for which TLS termination is enabled but the Secret is not specified.
		Format: <namespace>/<name>. If the argument is not set, for such Ingress/VirtualServer hosts NGINX will break any attempt to establish a TLS connection.
		If the argument is set, but the Ingress Controller is not able to fetch the Secret from Kubernetes API, the Ingress Controller will fail to start.`)

	enablePrometheusMetrics = flag.Bool("enable-prometheus-metrics", false,
		"Enable exposing NGINX or NGINX Plus metrics in the Prometheus format")

	prometheusTLSSecretName = flag.String("prometheus-tls-secret", "",
		`A Secret with a TLS certificate and key for TLS termination of the prometheus endpoint.`)

	prometheusMetricsListenPort = flag.Int("prometheus-metrics-listen-port", 9113,
		"Set the port where the Prometheus metrics are exposed. [1024 - 65535]")

	enableServiceInsight = flag.Bool("enable-service-insight", false,
		`Enable service insight for external load balancers. Requires -nginx-plus`)

	serviceInsightTLSSecretName = flag.String("service-insight-tls-secret", "",
		`A Secret with a TLS certificate and key for TLS termination of the service insight.`)

	serviceInsightListenPort = flag.Int("service-insight-listen-port", 9114,
		"Set the port where the Service Insight stats are exposed. Requires -nginx-plus. [1024 - 65535]")

	enableCustomResources = flag.Bool("enable-custom-resources", true,
		"Enable custom resources")

	enableGatewayAPI = flag.Bool("enable-gateway-api", false,
		"Serve the Gateway API: the GatewayClass -gateway-class if it names -gateway-controller-name, its Gateways and their HTTPRoutes. Requires -enable-custom-resources and the Gateway API CRDs.")

	gatewayControllerName = flag.String("gateway-controller-name", gateway.DefaultControllerName,
		"The controllerName of the GatewayClass to serve with -enable-gateway-api.")

	gatewayClass = flag.String("gateway-class", gateway.DefaultGatewayClass,
		"The name of the GatewayClass to serve with -enable-gateway-api.")

	defaultGateway = flag.String("default-gateway", "",
		`The Gateway, as <namespace>/<name>, that shares the address of the controller's Service (-external-service) and nginx's default ports with Ingresses and VirtualServers, for a migration from Ingress: its listeners may be HTTP on 80 and HTTPS on 443 only. Every other Gateway gets an address of its own, a Service the controller makes. Sharing an address with other resources is a deliberate deviation from the Gateway API.`)

	gatewayServiceType = flag.String("gateway-service-type", "LoadBalancer",
		"The type of the Services the controller makes for the Gateways with addresses of their own: LoadBalancer, NodePort or ClusterIP.")

	gatewayListenerPorts = flag.String("gateway-listener-ports", "20000-29999",
		"The range of ports, <first>-<last>, nginx listens on for the listeners of the Gateways with addresses of their own.")

	ingressClassControllers = flag.String("ingress-class-controllers", "nginx.wx1.eu/ingress-controller",
		`A comma-separated list of IngressClass controller values: Ingresses of every IngressClass whose spec.controller is one of them are served - k8s.io/ingress-nginx and nginx.org/ingress-controller run beside or in place of those controllers on their classes. The class of -ingress-class is served whether or not an IngressClass for it exists.`)

	watchIngressWithoutClass = flag.Bool("watch-ingress-without-class", false,
		`Serve Ingresses without a class (no ingressClassName, no kubernetes.io/ingress.class), as ingress-nginx's --watch-ingress-without-class. Without it they are served only when a served IngressClass is the cluster's default.`)

	tcpServicesConfigMap = flag.String("tcp-services-configmap", "",
		`A ConfigMap (<namespace>/<name>) of TCP services as ingress-nginx's --tcp-services-configmap: "<port>": "<namespace>/<service>:<port>[:PROXY][:PROXY]" - nginx listens on the port and passes the connections to the Service, with the first PROXY taking the PROXY protocol of the client and the second sending it to the Service. Watched, endpoints change without a reload with -dynamic-upstreams.`)

	udpServicesConfigMap = flag.String("udp-services-configmap", "",
		`A ConfigMap (<namespace>/<name>) of UDP services as ingress-nginx's --udp-services-configmap: "<port>": "<namespace>/<service>:<port>".`)

	defaultBackendService = flag.String("default-backend-service", "",
		`A Service (<namespace>/<name>) that answers, as ingress-nginx's --default-backend-service, the requests no Ingress has a host or a path for, and the custom-http-errors of Ingresses without a default-backend annotation. Its first port; its endpoints change without a reload with -dynamic-upstreams.`)

	enableOIDC = flag.Bool("enable-oidc", false,
		"Enable OIDC Policies.")

	oidcRevocationsConfigMap = flag.String("oidc-revocations-configmap", "",
		`With -enable-oidc and -dynamic-upstreams, the ConfigMap (<namespace>/<name>) whose "revocations" end oidc sessions of the wxingress module before their ID tokens do, and into which each replica writes the logouts its module took. No reload.`)

	enableSnippets = flag.Bool("enable-snippets", false,
		"Enable custom NGINX configuration snippets in Ingress, VirtualServer, VirtualServerRoute and TransportServer resources.")

	globalConfiguration = flag.String("global-configuration", "",
		`The namespace/name of the GlobalConfiguration resource for global configuration of the Ingress Controller. Requires -enable-custom-resources. Format: <namespace>/<name>`)

	enableTLSPassthrough = flag.Bool("enable-tls-passthrough", false,
		"Enable TLS Passthrough on default port 443. Requires -enable-custom-resources")

	tlsPassthroughPort = flag.Int("tls-passthrough-port", 443, "Set custom port for TLS Passthrough. [1024 - 65535]")

	readyStatus = flag.Bool("ready-status", true, "Enables the readiness endpoint '/nginx-ready'. The endpoint returns a success code when NGINX has loaded all the config after the startup")

	readyStatusPort = flag.Int("ready-status-port", 8081, "Set the port where the readiness endpoint is exposed. [1024 - 65535]")

	enableLatencyMetrics = flag.Bool("enable-latency-metrics", false,
		"Enable collection of latency metrics for upstreams. Requires -enable-prometheus-metrics")

	latencyMetricsBuckets = flag.String("latency-metrics-buckets", "",
		`Comma-separated list of buckets in milliseconds, in strictly ascending order, for the upstream_server_response_latency_ms histogram, e.g. "5,10,25,50,100,250,500,1000". Requires -enable-latency-metrics. When not set, a default set of buckets is used. Reducing the number of buckets lowers the metric's time-series cardinality.`)

	latencyBuckets []float64

	enableTopologyAwareRouting = flag.Bool("enable-topology-aware-routing", false,
		`Honor EndpointSlice topology hints (set by Service spec.trafficDistribution or the service.kubernetes.io/topology-mode annotation) when selecting upstream endpoints, preferring endpoints on the same node or in the same zone as the Ingress Controller pod.
		Spread Ingress Controller replicas across zones in proportion to client traffic, as each replica only routes to its own node or zone.
		The zone is read from the topology.kubernetes.io/zone label of the pod's node at startup; restart the Ingress Controller if that label changes.`)

	enableCertManager = flag.Bool("enable-cert-manager", false,
		"Enable cert-manager controller for VirtualServer resources. Requires -enable-custom-resources")

	enableExternalDNS = flag.Bool("enable-external-dns", false,
		"Enable external-dns controller for VirtualServer resources. Requires -enable-custom-resources")

	disableIPV6 = flag.Bool("disable-ipv6", false,
		`Disable IPV6 listeners explicitly for nodes that do not support the IPV6 stack`)

	defaultHTTPListenerPort = flag.Int("default-http-listener-port", 80, "Sets a custom port for the HTTP NGINX `default_server`. [1024 - 65535]")

	defaultHTTPSListenerPort = flag.Int("default-https-listener-port", 443, "Sets a custom port for the HTTPS `default_server`. [1024 - 65535]")

	enableDynamicSSLReload = flag.Bool(dynamicSSLReloadParam, true, "Enable reloading of SSL Certificates without restarting the NGINX process.")

	enableTelemetryReporting = flag.Bool("enable-telemetry-reporting", false, "Ignored: wxingress has no telemetry. Kept so that existing values and arguments keep working.")

	logFormat = flag.String("log-format", logFormatDefault, "Set log format to either glog, text, or json.")

	logLevel = flag.String("log-level", logLevelDefault,
		`Sets log level for Ingress Controller. Allowed values: fatal, error, warning, info, debug, trace.`)

	enableDynamicWeightChangesReload = flag.Bool(dynamicWeightChangesParam, false, "Enable changing weights of split clients without reloading NGINX. Requires -nginx-plus")

	enableDirectiveAutoadjust = flag.Bool("enable-directive-autoadjust", false, "Enable automatic adjustment of NGINX directives to avoid conflicting NGINX configuration. Results may vary and might not be ideal in all cases.")

	allowEmptyIngressHost = flag.Bool("allow-empty-ingress-host", false,
		`Allows Ingress resources to omit the host field. If multiple Ingress resources without a host conflict,
	NGINX Ingress Controller resolves the collision using the winner selection algorithm. To use multiple
	Ingress resources without a host across namespaces without conflict, use mergeable Ingress. To configure
	TLS, use -default-server-tls-secret. To configure listener ports, use -default-http-listener-port or
	-default-https-listener-port. Default is false.`)

	startupCheckFn func() error
)

// plmSecretRefs holds the parsed PLM secret refs after mustValidateFlags
// completes
type plmSecretRefs struct {
	Credentials types.NamespacedName
	CA          types.NamespacedName
	ClientSSL   types.NamespacedName
}

// plmRefs is populated by mustValidateFlags via validatePLMFlags. Zero-value
// when PLM is disabled (--plm-storage-url empty).
var plmRefs plmSecretRefs

//gocyclo:ignore
func parseFlags() {
	flag.Parse()

	if *versionFlag { // printed in main
		os.Exit(0)
	}
}

func initValidate(ctx context.Context) {
	l := nl.LoggerFromContext(ctx)
	logFormatValidationError := validateLogFormat(*logFormat)
	if logFormatValidationError != nil {
		nl.Warnf(l, "Invalid log format: %s. Valid options are: glog, text, json. Falling back to default: %s", *logFormat, logFormatDefault)
	}

	logLevelValidationError := validateLogLevel(*logLevel)
	if logLevelValidationError != nil {
		nl.Warnf(l, "Invalid log level: %s. Valid options are: trace, debug, info, warning, error, fatal. Falling back to default: %s", *logLevel, logLevelDefault)
	}

	if *enableLatencyMetrics && !*enablePrometheusMetrics {
		nl.Warn(l, "enable-latency-metrics flag requires enable-prometheus-metrics, latency metrics will not be collected")
		*enableLatencyMetrics = false
	}

	if *latencyMetricsBuckets != "" {
		switch {
		case !*enableLatencyMetrics:
			nl.Warn(l, "latency-metrics-buckets flag requires enable-latency-metrics, the configured buckets will be ignored")
		default:
			parsedBuckets, err := parseLatencyBuckets(*latencyMetricsBuckets)
			if err != nil {
				nl.Warnf(l, "Invalid latency-metrics-buckets value %q: %v. Falling back to the default latency buckets", *latencyMetricsBuckets, err)
			} else {
				latencyBuckets = parsedBuckets
			}
		}
	}

	if *enableServiceInsight && !*nginxPlus {
		nl.Warn(l, "enable-service-insight flag support is for NGINX Plus, service insight endpoint will not be exposed")
		*enableServiceInsight = false
	}

	if *enableDynamicWeightChangesReload && !*nginxPlus {
		nl.Warn(l, "weight-changes-dynamic-reload flag support is for NGINX Plus, Dynamic Weight Changes will not be enabled")
		*enableDynamicWeightChangesReload = false
	}

	if *syncRateLimit < 0 {
		nl.Warn(l, "sync-rate-limit cannot be negative, reloads will not be limited")
		*syncRateLimit = 0
	}

	if *dynamicUpstreams && *nginxPlus {
		nl.Warn(l, "dynamic-upstreams flag is for NGINX OSS, NGINX Plus updates upstreams through its own API")
		*dynamicUpstreams = false
	}

	if *mgmtConfigMap != "" && !*nginxPlus {
		nl.Warn(l, "mgmt-configmap flag requires -nginx-plus, mgmt configmap will not be used")
		*mgmtConfigMap = ""
	}

	mustValidateInitialChecks(ctx)
	mustValidateWatchedNamespaces(ctx)
	mustValidateFlags(ctx)
}

func mustValidateInitialChecks(ctx context.Context) {
	l := nl.LoggerFromContext(ctx)

	if startupCheckFn != nil {
		err := startupCheckFn()
		if err != nil {
			nl.Fatalf(l, "Failed startup check: %v", err)
		}
		l.Info("AWS startup check passed")
	}

	l.Info(fmt.Sprintf("Starting with flags: %+q", os.Args[1:]))

	unparsed := flag.Args()
	if len(unparsed) > 0 {
		nl.Warnf(l, "Ignoring unhandled arguments: %+q", unparsed)
	}
}

// mustValidateWatchedNamespaces calls internally os.Exit if it can't validate namespaces.
func mustValidateWatchedNamespaces(ctx context.Context) {
	l := nl.LoggerFromContext(ctx)
	if *watchNamespace != "" && *watchNamespaceLabel != "" {
		nl.Fatal(l, "watch-namespace and -watch-namespace-label are mutually exclusive")
	}

	watchNamespaces = strings.Split(*watchNamespace, ",")

	if *watchNamespace != "" {
		l.Info(fmt.Sprintf("Namespaces watched: %v", watchNamespaces))
		namespacesNameValidationError := validateNamespaceNames(watchNamespaces)
		if namespacesNameValidationError != nil {
			nl.Fatalf(l, "Invalid values for namespaces: %v", namespacesNameValidationError)
		}
	}

	if len(*watchSecretNamespace) > 0 {
		watchSecretNamespaces = strings.Split(*watchSecretNamespace, ",")
		l.Debug(fmt.Sprintf("Namespaces watched for secrets: %v", watchSecretNamespaces))
		namespacesNameValidationError := validateNamespaceNames(watchSecretNamespaces)
		if namespacesNameValidationError != nil {
			nl.Fatalf(l, "Invalid values for secret namespaces: %v", namespacesNameValidationError)
		}
	} else {
		// empty => default to watched namespaces
		watchSecretNamespaces = watchNamespaces
	}

	if *watchNamespaceLabel != "" {
		var err error
		_, err = labels.Parse(*watchNamespaceLabel)
		if err != nil {
			nl.Fatalf(l, "Unable to parse label %v for watch namespace label: %v", *watchNamespaceLabel, err)
		}
	}
}

// mustValidateFlags checks the values for various flags
// and calls os.Exit if any of the flags is invalid.
// nolint:gocyclo
func mustValidateFlags(ctx context.Context) {
	l := nl.LoggerFromContext(ctx)
	healthStatusURIValidationError := validateLocation(*healthStatusURI)
	if healthStatusURIValidationError != nil {
		nl.Fatalf(l, "Invalid value for health-status-uri: %v", healthStatusURIValidationError)
	}

	if err := validateAppProtectEnforcerAddress(*appProtectEnforcerAddress); err != nil {
		nl.Fatalf(l, "Invalid value for app-protect-enforcer-address: %v", err)
	}

	statusLockNameValidationError := validateResourceName(*leaderElectionLockName)
	if statusLockNameValidationError != nil {
		nl.Fatalf(l, "Invalid value for leader-election-lock-name: %v", statusLockNameValidationError)
	}

	statusPortValidationError := internalValidation.ValidateUnprivilegedPort(*nginxStatusPort)
	if statusPortValidationError != nil {
		nl.Fatalf(l, "Invalid value for nginx-status-port: %v", statusPortValidationError)
	}

	metricsPortValidationError := internalValidation.ValidateUnprivilegedPort(*prometheusMetricsListenPort)
	if metricsPortValidationError != nil {
		nl.Fatalf(l, "Invalid value for prometheus-metrics-listen-port: %v", metricsPortValidationError)
	}

	readyStatusPortValidationError := internalValidation.ValidateUnprivilegedPort(*readyStatusPort)
	if readyStatusPortValidationError != nil {
		nl.Fatalf(l, "Invalid value for ready-status-port: %v", readyStatusPortValidationError)
	}

	healthProbePortValidationError := internalValidation.ValidateUnprivilegedPort(*serviceInsightListenPort)
	if healthProbePortValidationError != nil {
		nl.Fatalf(l, "Invalid value for service-insight-listen-port: %v", metricsPortValidationError)
	}

	var err error
	allowedCIDRs, err = parseNginxStatusAllowCIDRs(*nginxStatusAllowCIDRs)
	if err != nil {
		nl.Fatalf(l, "Invalid value for nginx-status-allow-cidrs: %v", err)
	}

	if *appProtectLogLevel != appProtectLogLevelDefault && *appProtect && *nginxPlus {
		appProtectLogLevelValidationError := validateLogLevel(*appProtectLogLevel)
		if appProtectLogLevelValidationError != nil {
			nl.Fatalf(l, "Invalid value for app-protect-log-level: %v", *appProtectLogLevel)
		}
	}

	if *enableTLSPassthrough && !*enableCustomResources {
		nl.Fatal(l, "enable-tls-passthrough flag requires -enable-custom-resources")
	}

	if *appProtect && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect support is for NGINX Plus only")
	}

	if *appProtectLogLevel != appProtectLogLevelDefault && !*appProtect && !*nginxPlus {
		nl.Fatal(l, "app-protect-log-level support is for NGINX Plus only and App Protect is enable")
	}

	if *appProtectDos && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect Dos support is for NGINX Plus only")
	}

	if *appProtectDosDebug && !*appProtectDos && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect Dos debug support is for NGINX Plus only and App Protect Dos is enable")
	}

	if *appProtectDosMaxDaemons != 0 && !*appProtectDos && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect Dos max daemons support is for NGINX Plus only and App Protect Dos is enable")
	}

	if *appProtectDosMaxWorkers != 0 && !*appProtectDos && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect Dos max workers support is for NGINX Plus and App Protect Dos is enable")
	}

	if *appProtectDosMemory != 0 && !*appProtectDos && !*nginxPlus {
		nl.Fatal(l, "NGINX App Protect Dos memory support is for NGINX Plus and App Protect Dos is enable")
	}

	refs, warn, err := validatePLMFlags(
		*plmStorageURL,
		*plmStorageCredentialsSecret,
		*plmStorageCASecret,
		*plmStorageClientSSLSecret,
		*plmStorageInsecureSkipVerify,
		*nginxPlus,
		*appProtect,
	)
	if err != nil {
		nl.Fatal(l, err.Error())
	}
	if warn != "" {
		nl.Warn(l, warn)
	}
	plmRefs = refs

	for name, value := range map[string]string{"tcp-services-configmap": *tcpServicesConfigMap, "udp-services-configmap": *udpServicesConfigMap, "default-backend-service": *defaultBackendService} {
		if value != "" {
			if _, _, err := k8s.ParseNamespaceName(value); err != nil {
				nl.Fatalf(l, "Invalid value for %s: %v", name, err)
			}
		}
	}

	if *enableGatewayAPI && !*enableCustomResources {
		nl.Fatal(l, "enable-gateway-api flag requires -enable-custom-resources")
	}

	if *enableGatewayAPI {
		if *defaultGateway != "" {
			if _, _, err := k8s.ParseNamespaceName(*defaultGateway); err != nil {
				nl.Fatalf(l, "Invalid value for default-gateway: %v", err)
			}
		}
		switch api_v1.ServiceType(*gatewayServiceType) {
		case api_v1.ServiceTypeLoadBalancer, api_v1.ServiceTypeNodePort, api_v1.ServiceTypeClusterIP:
		default:
			nl.Fatalf(l, "Invalid value for gateway-service-type: %q is not LoadBalancer, NodePort or ClusterIP", *gatewayServiceType)
		}
		first, last, err := parsePortRange(*gatewayListenerPorts)
		if err != nil {
			nl.Fatalf(l, "Invalid value for gateway-listener-ports: %v", err)
		}
		for _, p := range gatewayReservedPorts() {
			if p >= first && p <= last {
				nl.Fatalf(l, "Invalid value for gateway-listener-ports: %d-%d holds port %d, which nginx or the controller listens on otherwise", first, last, p)
			}
		}
	}

	if *enableCertManager && !*enableCustomResources {
		nl.Fatal(l, "enable-cert-manager flag requires -enable-custom-resources")
	}

	if *enableExternalDNS && !*enableCustomResources {
		nl.Fatal(l, "enable-external-dns flag requires -enable-custom-resources")
	}

	if *ingressLink != "" && *externalService != "" {
		nl.Fatal(l, "ingresslink and external-service cannot both be set")
	}

	if *nginxPlus && *mgmtConfigMap == "" {
		nl.Fatal(l, "NGINX Plus requires a mgmt ConfigMap to be set")
	}
}

// validateNamespaceNames validates the namespaces are in the correct format
func validateNamespaceNames(namespaces []string) error {
	var allErrs []error

	for _, ns := range namespaces {
		if ns != "" {
			ns = strings.TrimSpace(ns)
			err := validateResourceName(ns)
			if err != nil {
				allErrs = append(allErrs, err)
				fmt.Printf("error %v ", err)
			}
		}
	}
	if len(allErrs) > 0 {
		return fmt.Errorf("errors validating namespaces: %v", allErrs)
	}
	return nil
}

// validateResourceName validates the name of a resource
func validateResourceName(name string) error {
	allErrs := validation.IsDNS1123Subdomain(name)
	if len(allErrs) > 0 {
		return fmt.Errorf("invalid resource name %v: %v", name, allErrs)
	}
	return nil
}

// validateLogLevel makes sure a given logLevel is one of the allowed values
func validateLogLevel(logLevel string) error {
	switch strings.ToLower(logLevel) {
	case
		"fatal",
		"error",
		"warn",
		"info",
		"debug",
		"trace":
		return nil
	}
	return fmt.Errorf("invalid log level: %v", logLevel)
}

// parseLatencyBuckets parses a comma-separated list of latency histogram buckets
// in milliseconds. The values must be positive and listed in strictly ascending
// order, as required by Prometheus histogram bucket bounds.
func parseLatencyBuckets(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	buckets := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, fmt.Errorf("empty bucket value in %q", s)
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("could not parse %q as a number: %w", p, err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("bucket values must be finite, got %q", p)
		}
		if v <= 0 {
			return nil, fmt.Errorf("bucket values must be positive, got %v", v)
		}
		if len(buckets) > 0 && v <= buckets[len(buckets)-1] {
			return nil, fmt.Errorf("bucket values must be in strictly ascending order, got %v after %v", v, buckets[len(buckets)-1])
		}
		buckets = append(buckets, v)
	}
	return buckets, nil
}

// validateLogFormat makes sure a given logFormat is one of the allowed values
func validateLogFormat(logFormat string) error {
	switch strings.ToLower(logFormat) {
	case "glog", "json", "text", "json-unix", "json-unix-ms", "text-unix", "text-unix-ms":
		return nil
	}
	return fmt.Errorf("invalid log format: %v", logFormat)
}

// validatePLMSecretRef parses a --plm-storage-*-secret value into a
// NamespacedName. Empty is accepted (returns zero value). Non-empty must be
// "namespace/name" with DNS-1123 subdomain parts.
func validatePLMSecretRef(flagName, value string) (types.NamespacedName, error) {
	if value == "" {
		return types.NamespacedName{}, nil
	}
	ns, name, err := helpers.ParseNamespaceName(value)
	if err != nil {
		return types.NamespacedName{}, fmt.Errorf("%s: %w", flagName, err)
	}
	if errs := validation.IsDNS1123Subdomain(ns); len(errs) > 0 {
		return types.NamespacedName{}, fmt.Errorf("%s: invalid namespace %q: %s", flagName, ns, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return types.NamespacedName{}, fmt.Errorf("%s: invalid name %q: %s", flagName, name, strings.Join(errs, "; "))
	}
	return types.NamespacedName{Namespace: ns, Name: name}, nil
}

// validatePLMStorageURL parses the --plm-storage-url value and
// returns an error if it is not a valid URL with a scheme and host.
func validatePLMStorageURL(rawURL string) error {
	if rawURL == "" {
		return errors.New("plm-storage-url: must not be empty")
	}
	if strings.ContainsAny(rawURL, " \t\r\n") {
		return fmt.Errorf("plm-storage-url: must not contain whitespace or control characters, got %q", rawURL)
	}
	if !strings.Contains(rawURL, "://") {
		return fmt.Errorf("plm-storage-url: scheme is required (http:// or https://), got %q", rawURL)
	}
	if err := internalValidation.ValidateURI(rawURL); err != nil {
		return fmt.Errorf("plm-storage-url: %w", err)
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("plm-storage-url: %w", err)
	}
	if parsed.Host == "" {
		return fmt.Errorf("plm-storage-url: host must not be empty in %q", rawURL)
	}
	return nil
}

// validatePLMFlags cross-validates the PLM flag family. --plm-storage-url is
// the master switch: when empty, any auxiliary PLM flag being set is fatal.
// All PLM flag validation (URL syntax, secret ref format) happens here.
func validatePLMFlags(rawURL, credentialsSecret, caSecret, clientSSLSecret string, insecureSkipVerify, nginxPlus, appProtect bool) (plmSecretRefs, string, error) {
	refs := plmSecretRefs{}
	secretFlags := [...]struct {
		name, val string
		dst       *types.NamespacedName
	}{
		{"plm-storage-credentials-secret", credentialsSecret, &refs.Credentials},
		{"plm-storage-ca-secret", caSecret, &refs.CA},
		{"plm-storage-client-ssl-secret", clientSSLSecret, &refs.ClientSSL},
	}

	// PLM disabled: no auxiliary flag may be set (guards against silent typos
	// that would otherwise leave PLM off while the operator thinks it's on).
	if rawURL == "" {
		for _, sec := range secretFlags {
			if sec.val != "" {
				return plmSecretRefs{}, "", fmt.Errorf("%s is set but plm-storage-url is not; PLM auxiliary flags require plm-storage-url", sec.name)
			}
		}
		if insecureSkipVerify {
			return plmSecretRefs{}, "", errors.New("plm-storage-insecure-skip-verify is set but plm-storage-url is not; PLM auxiliary flags require plm-storage-url")
		}
		return plmSecretRefs{}, "", nil
	}

	// PLM enabled: check prerequisites, validate the URL, then parse each ref.
	switch {
	case !nginxPlus:
		return plmSecretRefs{}, "", errors.New("plm-storage-url requires -nginx-plus")
	case !appProtect:
		return plmSecretRefs{}, "", errors.New("plm-storage-url requires -enable-app-protect")
	case credentialsSecret == "":
		return plmSecretRefs{}, "", errors.New("plm-storage-credentials-secret must be set when plm-storage-url is set")
	}
	if err := validatePLMStorageURL(rawURL); err != nil {
		return plmSecretRefs{}, "", err
	}
	for _, sec := range secretFlags {
		ref, err := validatePLMSecretRef(sec.name, sec.val)
		if err != nil {
			return plmSecretRefs{}, "", err
		}
		*sec.dst = ref
	}

	if insecureSkipVerify {
		return refs, "plm-storage-insecure-skip-verify is enabled; TLS verification of the PLM SeaweedFS filer will be skipped. This is for dev/test only.", nil
	}
	return refs, "", nil
}

// parseNginxStatusAllowCIDRs converts a comma separated CIDR/IP address string into an array of CIDR/IP addresses.
// It returns an array of the valid CIDR/IP addresses or an error if given an invalid address.
func parseNginxStatusAllowCIDRs(input string) (cidrs []string, err error) {
	cidrsArray := strings.Split(input, ",")
	for _, cidr := range cidrsArray {
		trimmedCidr := strings.TrimSpace(cidr)
		err := validateCIDRorIP(trimmedCidr)
		if err != nil {
			return cidrs, err
		}
		cidrs = append(cidrs, trimmedCidr)
	}
	return cidrs, nil
}

// validateCIDRorIP makes sure a given string is either a valid CIDR block or IP address.
// It an error if it is not valid.
func validateCIDRorIP(cidr string) error {
	if cidr == "" {
		return fmt.Errorf("invalid CIDR address: an empty string is an invalid CIDR block or IP address")
	}
	_, _, err := net.ParseCIDR(cidr)
	if err == nil {
		return nil
	}
	ip := net.ParseIP(cidr)
	if ip == nil {
		return fmt.Errorf("invalid IP address: %v", cidr)
	}
	return nil
}

func validateAppProtectEnforcerAddress(address string) error {
	return internalValidation.ValidateDirectiveToken(address)
}

const (
	locationFmt    = `/[^\s{};]*`
	locationErrMsg = "must start with / and must not include any whitespace character, `{`, `}` or `;`"
)

var locationRegexp = regexp.MustCompile("^" + locationFmt + "$")

func validateLocation(location string) error {
	if location == "" || location == "/" {
		return fmt.Errorf("invalid location format: '%v' is an invalid location", location)
	}
	if !locationRegexp.MatchString(location) {
		msg := validation.RegexError(locationErrMsg, locationFmt, "/path", "/path/subpath-123")
		return fmt.Errorf("invalid location format: %v", msg)
	}
	return nil
}

// parsePortRange parses <first>-<last>.
func parsePortRange(v string) (int32, int32, error) {
	a, b, ok := strings.Cut(v, "-")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not <first>-<last>", v)
	}
	first, err1 := strconv.Atoi(strings.TrimSpace(a))
	last, err2 := strconv.Atoi(strings.TrimSpace(b))
	if err1 != nil || err2 != nil || first < 1024 || last > 65535 || first > last {
		return 0, 0, fmt.Errorf("%q is no range of ports in 1024-65535", v)
	}
	return int32(first), int32(last), nil //nolint:gosec // in range
}

// gatewayReservedPorts are the ports nginx and the controller listen on
// other than for Gateways.
func gatewayReservedPorts() []int32 {
	ports := []int{
		*defaultHTTPListenerPort, *defaultHTTPSListenerPort, *nginxStatusPort, *prometheusMetricsListenPort,
		*serviceInsightListenPort, *readyStatusPort, *tlsPassthroughPort,
	}
	out := make([]int32, 0, len(ports))
	for _, p := range ports {
		out = append(out, int32(p)) //nolint:gosec // validated ports
	}
	return out
}
