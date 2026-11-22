package configs

import (
	"context"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/nginx"
)

// defaultServerReturnDefault is what the default server answers without default-server-return.
const defaultServerReturnDefault = "404"

// ConfigParams holds NGINX configuration parameters that affect the main NGINX config
// as well as configs for Ingress resources.
type ConfigParams struct {
	AddHeaderInherit                       string
	AppRoot                                string
	Context                                context.Context
	ClientMaxBodySize                      string
	ClientBodyBufferSize                   string
	DefaultServerAccessLogOff              bool
	DefaultServerReturn                    string
	FailTimeout                            string
	HealthCheckEnabled                     bool
	HealthCheckMandatory                   bool
	HealthCheckMandatoryQueue              int64
	HSTS                                   bool
	HSTSBehindProxy                        bool
	HSTSIncludeSubdomains                  bool
	HSTSMaxAge                             int64
	HSTSPreload                            bool
	HTTP2                                  bool
	Keepalive                              int
	LBMethod                               string
	LocationSnippets                       []string
	MainAccessLog                          string
	MainAddHeaders                         []version2.AddHeader
	DisableForwardedHeaders                bool
	UseForwardedHeaders                    bool
	MainErrorLogLevel                      string
	MainHTTPSnippets                       []string
	MainKeepaliveRequests                  int64
	MainKeepaliveTimeout                   string
	MainLogFormat                          []string
	MainLogFormatEscaping                  string
	MainMainSnippets                       []string
	MainOtelLoadModule                     bool
	MainOtelTraceInHTTP                    bool
	MainOtelExporterEndpoint               string
	MainOtelExporterHeaderName             string
	MainOtelExporterHeaderValue            string
	MainOtelServiceName                    string
	MainOtelTraceContext                   string
	MainServerNamesHashBucketSize          string
	MainServerNamesHashMaxSize             string
	MainStreamLogFormat                    []string
	MainStreamLogFormatEscaping            string
	MainStreamSnippets                     []string
	MainMapHashBucketSize                  string
	MainMapHashMaxSize                     string
	MainWorkerConnections                  string
	MainWorkerCPUAffinity                  string
	MainWorkerProcesses                    string
	MainWorkerRlimitNofile                 string
	MainWorkerShutdownTimeout              string
	MainClientBodyBufferSize               string
	MaxConns                               int
	MaxFails                               int
	AppProtectEnable                       string
	AppProtectPolicy                       string
	AppProtectLogConf                      string
	AppProtectLogEnable                    string
	MainAppProtectFailureModeAction        string
	MainAppProtectCompressedRequestsAction string
	MainAppProtectCookieSeed               string
	MainAppProtectCPUThresholds            string
	MainAppProtectPhysicalMemoryThresholds string
	MainAppProtectReconnectPeriod          string
	AppProtectDosResource                  string
	MainAppProtectDosLogFormat             []string
	MainAppProtectDosLogFormatEscaping     string
	MainAppProtectDosArbFqdn               string
	OIDC                                   OIDC
	ProxyBuffering                         bool
	ProxyBuffers                           string
	ProxyBufferSize                        string
	ProxyBusyBuffersSize                   string
	ProxyConnectTimeout                    string
	AddHeaders                             []version2.AddHeader
	ProxyHideHeaders                       []string
	ProxyMaxTempFileSize                   string
	ProxyRequestBuffering                  string
	ProxyPassHeaders                       []string
	ProxySetHeaders                        []version2.Header
	ProxyProtocol                          bool
	ProxyReadTimeout                       string
	ProxySendTimeout                       string
	ProxyNextUpstream                      string
	ProxyNextUpstreamTimeout               string
	ProxyNextUpstreamTries                 *uint64
	ProxyRedirectFrom                      string
	ProxyRedirectTo                        string
	ProxyHTTPVersion                       string
	CustomHTTPErrors                       []int
	RedirectToHTTPS                        bool
	HTTPRedirectCode                       int
	ResolverAddresses                      []string
	ResolverIPV6                           bool
	ResolverTimeout                        string
	ResolverValid                          string
	ServerSnippets                         []string
	// GlobalAccessRules are the allow and deny directives of the ConfigMap's
	// denylist-source-range and allowlist-source-range (ingress-nginx's, with
	// -ingress-nginx-defaults), in every server of an Ingress or VirtualServer
	// whatever its snippets; the source ranges of an Ingress replace them in
	// its locations, as in ingress-nginx.
	GlobalAccessRules            []string
	ServerTokens                 string
	ServerSSLCiphers             string
	ServerSSLPreferServerCiphers bool
	SlowStart                    string
	SSLRedirect                  bool
	UpstreamZoneSize             string
	UseClusterIP                 bool
	VariablesHashBucketSize      uint64
	VariablesHashMaxSize         uint64
	ZoneSync                     ZoneSync

	// The rewrite of ingress-nginx: rewrite-target, use-regex and x-forwarded-prefix.
	// IngressNginxDefaults tells that the defaults are those of ingress-nginx
	// (-ingress-nginx-defaults): its headers to the upstream and WebSocket in every location.
	IngressNginxDefaults bool

	IngressNginxRewriteTarget string
	// IngressNginxRedirect is the permanent-redirect or temporal-redirect of ingress-nginx.
	IngressNginxRedirect         *version2.Return
	IngressNginxUseRegex         bool
	IngressNginxXForwardedPrefix string

	// The rate limit of ingress-nginx, and its limit-rate and limit-rate-after.
	IngressNginxRateLimit      *version1.IngressNginxRateLimit
	IngressNginxLimitRate      string
	IngressNginxLimitRateAfter string

	// The basic auth of ingress-nginx, and if its locations answer 503 instead.
	IngressNginxBasicAuth *version1.BasicAuth
	IngressNginxDenied    bool
	// IngressNginxAccess are the allow/deny rules of the source-range annotations of ingress-nginx.
	IngressNginxAccess []string
	// from force-ssl-redirect, preserve-trailing-slash, enable-access-log and server-alias of ingress-nginx
	IngressNginxForceSSLRedirect      bool
	IngressNginxPreserveTrailingSlash bool
	IngressNginxAccessLogOff          bool
	IngressNginxServerAliases         []string
	IngressNginxCORS                  *version1.IngressNginxCORS
	IngressNginxAuthTLS               *IngressNginxClientCert
	IngressNginxErrorCodes            []int
	IngressNginxMirror                *version1.IngressNginxMirror
	IngressNginxProxySSL              *version2.EgressMTLS
	IngressNginxDirectives            []string
	// IngressNginxConnection is connection-proxy-header, IngressNginxCustomHeaders custom-headers,
	// IngressNginxOtel the opentelemetry annotations, IngressNginxStreamSnippet stream-snippet.
	IngressNginxConnection    string
	IngressNginxCustomHeaders []version2.Header
	IngressNginxOtel          *version1.IngressNginxOtel
	IngressNginxStreamSnippet []string
	IngressNginxFastCGI       *version1.IngressNginxFastCGI
	// IngressNginxMetrics: the locations count ingress-nginx's request metrics (StaticConfigParams.IngressNginxMetrics).
	IngressNginxMetrics bool
	// SharedUpstreamZone puts the upstreams into SharedUpstreamZone (the wxingress module).
	SharedUpstreamZone bool
	// IngressNginxAllowedResponseHeaders is the ConfigMap's global-allowed-response-headers:
	// the headers custom-headers may set, as in ingress-nginx.
	IngressNginxAllowedResponseHeaders []string
	// IngressNginxExternalAuth is the external auth of ingress-nginx, auth-url and the annotations that go with it.
	IngressNginxExternalAuth *ingressNginxExternalAuth
	// IngressNginxAffinity is the cookie affinity of ingress-nginx, affinity and the session-cookie annotations.
	IngressNginxAffinity *ingressNginxAffinity
	// IngressNginxBalancing is the load balancing of ingress-nginx, upstream-hash-by and load-balance.
	IngressNginxBalancing *ingressNginxBalancing

	RealIPHeader    string
	RealIPRecursive bool
	SetRealIPFrom   []string

	MainServerSSLCiphers             string
	MainServerSSLDHParam             string
	MainServerSSLDHParamFileContent  *string
	MainServerSSLPreferServerCiphers bool
	MainServerSSLProtocols           string

	IngressTemplate         *string
	VirtualServerTemplate   *string
	MainTemplate            *string
	TransportServerTemplate *string

	JWTKey      string
	JWTLoginURL string
	JWTRealm    string
	JWTToken    string

	BasicAuthSecret string
	BasicAuthRealm  string

	Ports    []int
	SSLPorts []int

	LimitReqRate       string
	LimitReqKey        string
	LimitReqZoneSize   string
	LimitReqDelay      int
	LimitReqNoDelay    bool
	LimitReqBurst      int
	LimitReqDryRun     bool
	LimitReqLogLevel   string
	LimitReqRejectCode int
	LimitReqScale      bool
}

// StaticConfigParams holds immutable NGINX configuration parameters that affect the main NGINX config.
type StaticConfigParams struct {
	DisableIPV6                    bool
	DefaultHTTPListenerPort        int
	DefaultHTTPSListenerPort       int
	HealthStatus                   bool
	HealthStatusURI                string
	NginxStatus                    bool
	NginxStatusAllowCIDRs          []string
	NginxStatusPort                int
	StubStatusOverUnixSocketForOSS bool
	TLSPassthrough                 bool
	TLSPassthroughPort             int
	// IngressNginxMetrics has the wxingress module count ingress-nginx's request metrics (-enable-prometheus-metrics with -dynamic-upstreams).
	IngressNginxMetrics bool
	EnableSnippets      bool
	// SystemResolvers are the name servers of the pod, for what has to resolve per request without resolver-addresses.
	SystemResolvers              []string
	MainAppProtectLoadModule     bool
	MainAppProtectV5LoadModule   bool
	MainAppProtectDosLoadModule  bool
	MainAppProtectV5EnforcerAddr string
	EnableLatencyMetrics         bool
	EnableOIDC                   bool
	SSLRejectHandshake           bool
	EnableCertManager            bool
	DynamicSSLReload             bool
	StaticSSLPath                string
	DynamicWeightChangesReload   bool
	IsDirectiveAutoadjustEnabled bool
	NginxVersion                 nginx.Version
	AppProtectBundlePath         string
	DefaultCABundle              string
	// PLMEnabled reports whether WAF bundles are sourced from the F5 WAF Policy
	// Controller. When true, apPolicy/apLogConf references resolve to PLM bundles
	// instead of in-pod compiled App Protect resources.
	PLMEnabled bool
	// UpstreamAPI reports whether NGINX OSS runs with the upstream API of the
	// wxingress module (-dynamic-upstreams).
	UpstreamAPI bool
	// UpstreamAPITokenFile is the file with the check of the controller's
	// token that the server of the upstream API includes; "" for none.
	UpstreamAPITokenFile string
}

// upstreamZoneSize is the size of an upstream's shared memory zone. The
// upstream API only reaches upstreams that have one, so with it a size of "0"
// - no zone - falls back to the default rather than to a reload for every
// endpoint change.
// SharedUpstreamZone and SharedUpstreamZoneSize: with the wxingress module
// (NGINX OSS, -dynamic-upstreams) every upstream of Ingresses and
// VirtualServers keeps its servers in this one zone, as nginx allows
// ("several groups may share the same zone"). A zone of its own costs every
// upstream the pages of its slab's size classes - some 30 kB each, touched,
// for a few hundred bytes of servers, 3 MB of every pod for a hundred
// upstreams; in one zone they share them. Its size is virtual until used and
// holds some hundred thousand servers; upstream-zone-size and its annotation
// do not apply to it.
const (
	SharedUpstreamZone     = "wxingress-upstreams"
	SharedUpstreamZoneSize = "64m"
)

func upstreamZoneSize(size string, upstreamAPI bool) string {
	if upstreamAPI && size == "0" {
		return "256k"
	}
	return size
}

// GlobalConfigParams holds global configuration parameters. For now, it only holds listeners.
// GlobalConfigParams should replace ConfigParams in the future.
type GlobalConfigParams struct {
	Listeners map[string]Listener
}

// Listener represents a listener that can be used in a TransportServer resource.
type Listener struct {
	Port     int
	Protocol string
}

// ZoneSync holds zone sync values for state sharing.
type ZoneSync struct {
	Enable            bool
	Port              int
	Domain            string
	ResolverAddresses []string
	ResolverValid     string
	ResolverIPV6      *bool
}

// OIDC holds OIDC configuration parameters.
type OIDC struct {
	PKCETimeout  string
	PKCEZoneSize string

	IDTokenTimeout  string
	IDTokenZoneSize string

	AccessTimeout  string
	AccessZoneSize string

	RefreshTimeout  string
	RefreshZoneSize string

	SIDSTimeout  string
	SIDSZoneSize string
}

// MGMTSecrets holds mgmt block secret names
type MGMTSecrets struct {
	License     string
	ClientAuth  string
	TrustedCert string
	TrustedCRL  string
}

// MGMTConfigParams holds mgmt block parameters.
type MGMTConfigParams struct {
	Context              context.Context
	SSLVerify            *bool
	ResolverAddresses    []string
	ResolverIPV6         *bool
	ResolverValid        string
	EnforceInitialReport *bool
	Endpoint             string
	Interval             string
	Secrets              MGMTSecrets
	ProxyHost            string
	ProxyUser            string
	ProxyPass            string
}

// NewDefaultConfigParams creates a ConfigParams with default values.
func NewDefaultConfigParams(ctx context.Context, isPlus bool) *ConfigParams {
	upstreamZoneSize := "256k"
	if isPlus {
		upstreamZoneSize = "512k"
	}

	return &ConfigParams{
		Context:                       ctx,
		DefaultServerReturn:           defaultServerReturnDefault,
		ServerTokens:                  "on",
		ProxyConnectTimeout:           "60s",
		ProxyReadTimeout:              "60s",
		ProxySendTimeout:              "60s",
		ClientMaxBodySize:             "1m",
		SSLRedirect:                   true,
		HTTPRedirectCode:              301,
		MainAccessLog:                 "/dev/stdout main",
		MainServerNamesHashBucketSize: "256",
		MainServerNamesHashMaxSize:    "1024",
		MainMapHashBucketSize:         "256",
		MainMapHashMaxSize:            "2048",
		ProxyBuffering:                true,
		MainWorkerProcesses:           "auto",
		MainWorkerConnections:         "1024",
		HSTSMaxAge:                    2592000,
		HSTSPreload:                   true,
		Ports:                         []int{80},
		SSLPorts:                      []int{443},
		MaxFails:                      1,
		MaxConns:                      0,
		UpstreamZoneSize:              upstreamZoneSize,
		FailTimeout:                   "10s",
		LBMethod:                      "random two least_conn",
		MainErrorLogLevel:             "notice",
		ResolverIPV6:                  true,
		MainKeepaliveTimeout:          "75s",
		MainKeepaliveRequests:         1000,
		VariablesHashBucketSize:       256,
		VariablesHashMaxSize:          1024,
		DisableForwardedHeaders:       false,
		UseForwardedHeaders:           false,
		LimitReqKey:                   "${binary_remote_addr}",
		LimitReqZoneSize:              "10m",
		LimitReqLogLevel:              "error",
		LimitReqRejectCode:            429,
		OIDC: OIDC{
			PKCETimeout:     "90s",
			PKCEZoneSize:    "128K",
			IDTokenTimeout:  "1h",
			IDTokenZoneSize: "1M",
			AccessTimeout:   "1h",
			AccessZoneSize:  "1M",
			RefreshTimeout:  "8h",
			RefreshZoneSize: "1M",
			SIDSTimeout:     "8h",
			SIDSZoneSize:    "1M",
		},
	}
}

// NewDefaultMGMTConfigParams creates a ConfigParams with mgmt values.
func NewDefaultMGMTConfigParams(ctx context.Context) *MGMTConfigParams {
	return &MGMTConfigParams{
		Context:              ctx,
		SSLVerify:            nil,
		EnforceInitialReport: nil,
		Secrets:              MGMTSecrets{},
	}
}
