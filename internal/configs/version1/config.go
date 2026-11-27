package version1

import (
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/nginx"
)

// UpstreamLabels describes the Prometheus labels for an NGINX upstream.
type UpstreamLabels struct {
	Service           string
	ResourceType      string
	ResourceName      string
	ResourceNamespace string
}

// IngressNginxConfig describes an NGINX configuration.
type IngressNginxConfig struct {
	Upstreams               []Upstream
	Servers                 []Server
	Maps                    []version2.Map
	CORSHeaders             []version2.AddHeader
	OIDCProviders           []version2.OIDCProvider
	Ingress                 Ingress
	DynamicSSLReloadEnabled bool
	StaticSSLPath           string
	// DefaultCertificate is the certificate every TLS server names when the
	// certificates of hosts come from the wxingress module (-dynamic-upstreams):
	// the one a handshake gets whose name has none there.
	DefaultCertificate string
	LimitReqZones      []LimitReqZone
	// IngressNginxRateLimits are the rate limits of ingress-nginx of the Ingress and its minions.
	IngressNginxRateLimits []IngressNginxRateLimit
	// IngressNginxAuthCaches are the caches of the external auth of ingress-nginx of the Ingress and its minions.
	IngressNginxAuthCaches []IngressNginxAuthCache
	// IngressNginxAuthUpstreams are the upstreams with keepalive of the external auth of ingress-nginx (auth-keepalive).
	IngressNginxAuthUpstreams []IngressNginxAuthUpstream
	// IngressNginxStreamSnippets are the stream-snippet annotations of ingress-nginx of the Ingress and those merged into it,
	// for the stream block.
	IngressNginxStreamSnippets []string
	KeyValZones                []version2.KeyValZone
	LimitConnZones             []version2.LimitConnZone
	// AppProtectLoadModule mirrors the controller's -enable-app-protect flag so
	// templates can safely emit app_protect_enable off; in internal sub-request
	// locations only when the WAF module is actually loaded.
	AppProtectLoadModule bool
	// HTTP2 mirrors the http2 ConfigMap key, which is set in the http context. Servers with
	// HTTP/2 off only render http2 off; when this is on.
	HTTP2 bool
	// Canaries holds the canary rules of the locations, by their primary
	// upstream. They go to the wxingress module, not to the template.
	Canaries map[string]nginx.Canary
	// Balancers holds the balancer rules of the upstreams, by their name:
	// ingress-nginx's load-balance, upstream-hash-by and affinity. They go to
	// the wxingress module, not to the template.
	Balancers map[string]nginx.Balancer
}

// Ingress holds information about an Ingress resource.
type Ingress struct {
	Name        string
	Namespace   string
	Annotations map[string]string
}

// Upstream describes an NGINX upstream.
type Upstream struct {
	Name             string
	UpstreamServers  []UpstreamServer
	StickyCookie     string
	LBMethod         string
	Queue            int64
	QueueTimeout     int64
	UpstreamZoneSize string
	// Zone is the shared memory zone of the upstream when it is not its own (SharedUpstreamZone)
	Zone           string
	UpstreamLabels UpstreamLabels
	Keepalive      string
	// MetaBalancer has the wxingress module choose the server of each request
	// by the balancer rule of the upstream, if it has one (-dynamic-upstreams).
	MetaBalancer bool
}

// UpstreamServer describes a server in an NGINX upstream.
type UpstreamServer struct {
	Address     string
	MaxFails    int
	MaxConns    int
	FailTimeout string
	SlowStart   string
	Resolve     bool
}

// HealthCheck describes an active HTTP health check.
type HealthCheck struct {
	UpstreamName   string
	URI            string
	Interval       int32
	Fails          int32
	Passes         int32
	Scheme         string
	Mandatory      bool
	Headers        map[string]string
	TimeoutSeconds int64
}

// LimitReqZone describes a zone used for request rate limiting
type LimitReqZone struct {
	Name string
	Key  string
	Size string
	Rate string
	Sync bool
}

// Server describes an NGINX server.
type Server struct {
	AddHeaderInherit string
	ServerSnippets   []string
	// GlobalAccessRules are the allow/deny directives of the ConfigMap's source ranges.
	GlobalAccessRules []string
	Name              string
	IsDefaultServer   bool
	AccessLogOff      bool
	// ServerAliases are more names of the server (server-alias of ingress-nginx).
	ServerAliases []string
	// IngressNginxRedirect makes the redirect to https ingress-nginx's: https://<Host without port><URI without trailing slash>.
	IngressNginxRedirect bool
	// IngressNginxHTTPSRedirect: the server sends plain http to https as ingress-nginx does, in each location after
	// nginx checked the body's size and after the location's own returns, not in the server before them.
	IngressNginxHTTPSRedirect bool
	PreserveTrailingSlash     bool
	// IngressNginxMatchCN and IngressNginxClientCertErrorPage are auth-tls-match-cn and auth-tls-error-page.
	IngressNginxMatchCN             string
	IngressNginxClientCertErrorPage string
	// IngressNginxClientCertHTTPSOnly: auth-tls on a host whose https is the
	// default certificate's (no TLS of its own): plain http, which has no
	// client certificate to check, answers 503 rather than being served.
	IngressNginxClientCertHTTPSOnly bool
	// IngressNginxDefaultBackend is the upstream of -default-backend-service for the requests no location of
	// the server is for (a server without "/", the default server).
	IngressNginxDefaultBackend string
	// IngressNginxErrorLocations are the named locations of custom-http-errors and default-backend.
	IngressNginxErrorLocations []IngressNginxErrorLocation
	// IngressNginxMirrors are the internal locations the mirrors of the locations send to.
	IngressNginxMirrors []IngressNginxMirror
	// MirrorResolver resolves the hosts of the mirrors per request.
	MirrorResolver string
	// RedirectAllTo makes the server answer every request with a redirect to it and the URI (from-to-www-redirect).
	RedirectAllTo string
	// ForceSSLRedirect redirects plain http to https also without TLS on the server (force-ssl-redirect).
	ForceSSLRedirect       bool
	DefaultServerReturn    string
	HealthStatus           bool
	HealthStatusURI        string
	ServerTokens           string
	Locations              []Location
	EgressMTLS             *version2.EgressMTLS
	SSL                    bool
	SSLCertificate         string
	SSLCertificateKey      string
	SSLCiphers             string
	SSLPreferServerCiphers bool
	SSLRejectHandshake     bool
	TLSPassthrough         bool
	GRPCOnly               bool
	IngressMTLS            *version2.IngressMTLS
	HasGRPCLocations       bool
	StatusZone             string
	HTTP2                  bool
	RedirectToHTTPS        bool
	SSLRedirect            bool
	HTTPRedirectCode       int
	ProxyProtocol          bool
	HSTS                   bool
	HSTSMaxAge             int64
	HSTSIncludeSubdomains  bool
	HSTSPreload            bool
	HSTSBehindProxy        bool
	ProxyHideHeaders       []string
	ProxyPassHeaders       []string
	AddHeaders             []version2.AddHeader
	Allow                  []string
	Deny                   []string
	OIDCProviderName       string
	PoliciesErrorReturn    *version2.Return
	LimitConnOptions       version2.LimitConnOptions
	LimitConns             []version2.LimitConn

	HealthChecks map[string]HealthCheck

	RealIPHeader    string
	SetRealIPFrom   []string
	RealIPRecursive bool

	JWTAuth              *JWTAuth
	ExternalAuth         *version2.ExternalAuth
	BasicAuth            *BasicAuth
	JWTRedirectLocations []JWTRedirectLocation

	Ports                        []int
	SSLPorts                     []int
	AppProtectEnable             string
	AppProtectPolicy             string
	AppProtectLogConfs           []string
	AppProtectLogEnable          string
	AppProtectDosEnable          string
	AppProtectDosPolicyFile      string
	AppProtectDosLogConfFile     string
	AppProtectDosLogEnable       bool
	AppProtectDosMonitorURI      string
	AppProtectDosMonitorProtocol string
	AppProtectDosMonitorTimeout  uint64
	AppProtectDosName            string
	AppProtectDosAllowListPath   string
	AppProtectDosAccessLogDst    string
	WAF                          *version2.WAF

	DisableIPV6 bool

	ProxyRedirectFrom string
	ProxyRedirectTo   string

	// CustomHTTPErrorCodes holds the upstream status codes to intercept at server
	// context (set from the nginx.org/custom-http-errors annotation on the Ingress
	// or on the master of a mergeable Ingress). All non-overriding locations on
	// the server inherit these directives via standard NGINX inheritance.
	CustomHTTPErrorCodes []int
	// CustomHTTPErrorBackend is the upstream name that @custom_default_backend
	// proxies to. Populated from the Ingress's spec.defaultBackend upstream.
	// Non-empty iff the server should render the shared @custom_default_backend
	// named location — that is, custom-http-errors are configured AND the Ingress
	// has a spec.defaultBackend. Doubles as the enable flag; no separate boolean.
	CustomHTTPErrorBackend string
	// AuthResponseHeaderNames are the auth response headers that any ExternalAuth Policy of the server lists.
	// @custom_default_backend removes them from the requests it re-sends, and never forwards values.
	AuthResponseHeaderNames []string

	AppRoot string

	// ACMEChallengeActive is true when the server has at least one ACME HTTP-01 challenge location.
	ACMEChallengeActive bool
}

// JWTRedirectLocation describes a location for redirecting client requests to a login URL for JWT Authentication.
type JWTRedirectLocation struct {
	Name     string
	LoginURL string
}

// BasicAuth holds HTTP Basic authentication parameters
type BasicAuth struct {
	Realm  string
	Secret string
}

// JWTAuth holds JWT authentication configuration.
type JWTAuth struct {
	Key                  string
	Realm                string
	Token                string
	RedirectLocationName string
}

// LimitReq configures a request rate limit
type LimitReq struct {
	Zone       string
	Burst      int
	Delay      int
	NoDelay    bool
	RejectCode int
	DryRun     bool
	LogLevel   string
}

// IngressNginxRateLimit is the rate limit of an Ingress with the annotations
// of ingress-nginx, rendered as ingress-nginx does: clients of the allowlist
// have an empty key and are not limited.
type IngressNginxRateLimit struct {
	// Name is namespace_name_uid of the Ingress; its zones are Name_conn, Name_rps and Name_rpm.
	Name string
	// ID makes the names of the variables $allowlist_ID and $limit_ID.
	ID string
	// Key is the key of a client that is not in the allowlist.
	Key         string
	Allowlist   []string
	Connections int
	RPS         int
	RPSBurst    int
	RPM         int
	RPMBurst    int
	// ZoneSize is the size of each zone.
	ZoneSize string
}

// IngressNginxExternalAuth is the external auth of ingress-nginx of a location:
// auth_request to an internal location that proxies to URL with the headers
// ingress-nginx sets, and a 401 that redirects to the sign-in URL, if any.
type IngressNginxExternalAuth struct {
	// Location is /_external-auth-<path in base64>-<path type>, the internal location.
	Location string
	URL      string
	// Resolver is the resolver of the internal location, unless the http block has one: nginx resolves URL per request.
	Resolver        string
	Host            string
	Method          string
	RequestRedirect string
	ResponseHeaders []IngressNginxAuthHeader
	Snippet         string
	AlwaysSetCookie bool
	// SigninLocation is the named location that redirects to SigninURL, if any.
	SigninLocation string
	SigninURL      string
	// CacheZone, CacheKey and CacheDurations cache the answers of the auth service.
	CacheZone      string
	CacheKey       string
	CacheDurations []string
	// ProxySetHeaders are the headers of auth-proxy-set-headers, sorted by name.
	ProxySetHeaders []version2.Header
	// Upstream is the IngressNginxAuthUpstream URL goes to, with auth-keepalive.
	Upstream string
}

// IngressNginxAuthHeader is a header of the answer of the auth service that is passed to the backend.
type IngressNginxAuthHeader struct {
	Name string
	// Variable is the variable of nginx that holds the header in the answer, $upstream_http_<name>.
	Variable string
}

// IngressNginxAuthUpstream is the upstream of an auth service with keepalive
// connections, as ingress-nginx's auth-keepalive makes it.
type IngressNginxAuthUpstream struct {
	Name   string
	Server string
	// Resolve resolves Server's name while nginx runs (server ... resolve), with Resolver unless the http block has one:
	// an auth service that does not exist yet fails no reload.
	Resolve           bool
	Resolver          string
	Keepalive         int
	KeepaliveRequests int
	KeepaliveTimeout  int
}

// IngressNginxFastCGI is ingress-nginx's FastCGI of a location: fastcgi-index
// and the parameters of fastcgi-params-configmap, sorted by name.
type IngressNginxFastCGI struct {
	Index  string
	Params []version2.Header
}

// IngressNginxOtel is the tracing of a location with ingress-nginx's
// enable-opentelemetry, opentelemetry-trust-incoming-span and
// opentelemetry-operation-name, as directives of nginx's otel module.
type IngressNginxOtel struct {
	// Trace is otel_trace on or off.
	Trace bool
	// Context is otel_trace_context: propagate when incoming spans are trusted, inject when not.
	Context string
	// SpanName is otel_span_name, if any.
	SpanName string
}

// IngressNginxAuthCache is the cache of the answers of the auth services of an Ingress.
type IngressNginxAuthCache struct {
	Zone string
	Path string
}

// Location describes an NGINX location.
type Location struct {
	AddHeaderInherit        string
	LocationSnippets        []string
	Path                    string
	Upstream                Upstream
	ProxyPass               string
	ProxyPassRequestBody    string
	ProxyPassRequestHeaders string
	ProxyConnectTimeout     string
	ProxyReadTimeout        string
	ProxySendTimeout        string
	ProxySetHeaders         []version2.Header
	ClientMaxBodySize       string
	ClientBodyBufferSize    string
	Websocket               bool
	// IngressNginxProxyHeaders renders the headers ingress-nginx sends to the
	// upstream: WebSocket in every location, X-Request-ID, X-Forwarded-For the
	// client's address, X-Scheme, X-Forwarded-Scheme, X-Original-Forwarded-For
	// and an empty Proxy.
	IngressNginxProxyHeaders bool
	Rewrite                  string
	RewriteTarget            string
	UpstreamVhost            string
	SSL                      bool
	GRPC                     bool
	ProxyBuffering           bool
	ProxyBuffers             string
	ProxyBufferSize          string
	ProxyBusyBuffersSize     string
	ProxyMaxTempFileSize     string
	// ProxyRequestBuffering is on or off, or empty to leave proxy_request_buffering out.
	ProxyRequestBuffering   string
	ProxySSLName            string
	AddHeaders              []version2.AddHeader
	JWTAuth                 *JWTAuth
	ExternalAuth            *version2.ExternalAuth
	AuthResponseHeaders     *version2.AuthResponseHeaders // set only by the resolution pass
	BasicAuth               *BasicAuth
	ServiceName             string
	LimitReq                *LimitReq
	DisableForwardedHeaders bool
	UseForwardedHeaders     bool
	CORSEnabled             bool

	AuthRequestOff bool
	Internal       bool
	// DisableWAF marks subrequest targets such as the ExternalAuth location.
	DisableWAF bool

	MinionIngress *Ingress

	ProxyNextUpstream        string
	ProxyNextUpstreamTimeout string
	ProxyNextUpstreamTries   *uint64
	ProxyRedirectFrom        string
	ProxyRedirectTo          string
	ProxyHTTPVersion         string
	// CustomHTTPErrorCodes lists the upstream status codes to intercept at this
	// location. When non-empty, the location renders proxy_intercept_errors on;
	// and, when the parent Server has a non-empty CustomHTTPErrorBackend, an
	// error_page directive routing those codes to @custom_default_backend.
	// Overrides any server-level CustomHTTPErrorCodes for this location per
	// NGINX's error_page inheritance rule (nested block replaces parent).
	CustomHTTPErrorCodes []int
	// SkipCustomHTTPErrors is set on the synthesized default-backend location to
	// break the intercept loop when server-level custom-http-errors would
	// otherwise cause the default backend to intercept its own responses. When
	// true the template emits proxy_intercept_errors off; inside the location.
	SkipCustomHTTPErrors       bool
	ProxySSLVerify             bool
	ProxySSLVerifyDepth        int
	ProxySSLTrustedCertificate string
	Allow                      []string
	Deny                       []string
	// IngressNginxErrorPages are the codes of custom-http-errors with the named location each goes to.
	IngressNginxErrorPages []IngressNginxErrorLocation
	// IngressNginxFallback is the named location of default-backend for what nginx answers 502 or 503 itself.
	IngressNginxFallback string
	// IngressNginxDirectives are directives of ingress-nginx annotations, validated, in the order to render.
	IngressNginxDirectives []string
	// IngressNginxConnection is ingress-nginx's connection-proxy-header: the Connection header to the backend.
	IngressNginxConnection string
	// IngressNginxCustomHeaders are the response headers of ingress-nginx's custom-headers, sorted by name.
	IngressNginxCustomHeaders []version2.Header
	// IngressNginxOtel is the tracing of ingress-nginx's opentelemetry annotations.
	IngressNginxOtel *IngressNginxOtel
	// MetricsLabels make the wxingress module count the requests of the location as ingress-nginx's
	// metrics: "c" where the location has a canary, else "-", then its primary upstream, the namespace,
	// the Ingress, the Service and the path, separated by tabs (IngressNginxMetricsLabels).
	MetricsLabels string
	// IngressNginxFastCGI makes the location pass to its upstream with FastCGI (backend-protocol FCGI).
	IngressNginxFastCGI *IngressNginxFastCGI
	// IngressNginxMirror is the mirror of ingress-nginx's mirror-target.
	IngressNginxMirror *IngressNginxMirror
	// IngressNginxCORS is the CORS of ingress-nginx's enable-cors.
	IngressNginxCORS *IngressNginxCORS
	// AccessLogOff is enable-access-log: "false" of ingress-nginx.
	AccessLogOff bool
	// AccessRules are allow/deny directives in their order, from the source-range annotations of ingress-nginx.
	AccessRules         []string
	WAF                 *version2.WAF
	EgressMTLS          *version2.EgressMTLS
	OIDCProviderName    string
	PoliciesErrorReturn *version2.Return
	LimitConnOptions    version2.LimitConnOptions
	LimitConns          []version2.LimitConn
	// ACMEChallenge marks a location that serves a cert-manager ACME HTTP-01 challenge.
	ACMEChallenge bool
	// StrippedAuthResponseHeaders are the names that an ExternalAuth Policy of the server lists but the location
	// does not forward. The location removes the client's copies. Set only by the resolution pass.
	StrippedAuthResponseHeaders []string
	// Canary is the variable of the wxingress module that names Upstream or its canary (-dynamic-upstreams).
	Canary string
	// UseRegex tells that the location has use-regex or a rewrite-target of ingress-nginx,
	// which make every path of its server a regular expression.
	UseRegex bool
	// CaseInsensitiveRegex renders the path as ~* "^path", as ingress-nginx does in such a server.
	CaseInsensitiveRegex bool
	// CaseInsensitiveRewrite renders the pattern of RewriteTarget as "(?i)path", as ingress-nginx does.
	CaseInsensitiveRewrite bool
	// IngressNginxRateLimit is the rate limit of ingress-nginx of the location's Ingress.
	IngressNginxRateLimit *IngressNginxRateLimit
	// LimitRate and LimitRateAfter are limit_rate and limit_rate_after, or empty.
	LimitRate      string
	LimitRateAfter string
	// IngressNginxExternalAuth is the external auth of ingress-nginx of the location's Ingress.
	IngressNginxExternalAuth *IngressNginxExternalAuth
	// IngressNginxExternalAuthShared tells that another location of the same
	// path renders the internal locations of the external auth: the exact
	// location of a Prefix path, which ingress-nginx adds beside path/.
	IngressNginxExternalAuthShared bool
	// IngressNginxRedirect is the redirect of permanent-redirect or
	// temporal-redirect of ingress-nginx: the location answers with it.
	IngressNginxRedirect *version2.Return
}

// ZoneSyncConfig is tbe configuration for the zone_sync directives for state sharing.
type ZoneSyncConfig struct {
	Enable            bool
	Port              int
	Domain            string
	ResolverAddresses []string
	// Time the resolver is valid. Go time string format: "5s", "10s".
	ResolverValid string
	ResolverIPV6  *bool
}

// OIDCConfig allows to configure OIDC parameters.
type OIDCConfig struct {
	Enable          bool
	PKCETimeout     string
	PKCEZoneSize    string
	IDTokenTimeout  string
	IDTokenZoneSize string
	AccessTimeout   string
	AccessZoneSize  string
	RefreshTimeout  string
	RefreshZoneSize string
	SIDSTimeout     string
	SIDSZoneSize    string
}

// MGMTConfig is tbe configuration for the MGMT block.
type MGMTConfig struct {
	SSLVerify            *bool
	EnforceInitialReport *bool
	Endpoint             string
	Interval             string
	TrustedCert          bool
	TrustedCRL           bool
	ClientAuth           bool
	ResolverAddresses    []string
	ResolverIPV6         *bool
	ResolverValid        string
	ProxyHost            string
	ProxyUser            string
	ProxyPass            string
}

// MainConfig describe the main NGINX configuration file.
type MainConfig struct {
	AccessLog                          string
	AddHeaderInherit                   string
	DisableIPV6                        bool
	ErrorLogLevel                      string
	HTTP2                              bool
	HTTPSnippets                       []string
	AddHeaders                         []version2.AddHeader
	KeepaliveRequests                  int64
	KeepaliveTimeout                   string
	LogFormat                          []string
	LogFormatEscaping                  string
	MainSnippets                       []string
	MGMTConfig                         MGMTConfig
	NginxStatus                        bool
	NginxStatusAllowCIDRs              []string
	NginxStatusPort                    int
	MainOtelLoadModule                 bool
	MainOtelGlobalTraceEnabled         bool
	MainOtelExporterEndpoint           string
	MainOtelExporterHeaderName         string
	MainOtelExporterHeaderValue        string
	MainOtelServiceName                string
	MainOtelTraceContext               string
	ProxyProtocol                      bool
	ResolverAddresses                  []string
	ResolverIPV6                       bool
	ResolverTimeout                    string
	ResolverValid                      string
	SetRealIPFrom                      []string
	ServerNamesHashBucketSize          string
	ServerNamesHashMaxSize             string
	MapHashBucketSize                  string
	MapHashMaxSize                     string
	ClientBodyBufferSize               string
	SSLCiphers                         string
	SSLDHParam                         string
	SSLPreferServerCiphers             bool
	SSLProtocols                       string
	StreamLogFormat                    []string
	StreamLogFormatEscaping            string
	StreamSnippets                     []string
	StubStatusOverUnixSocketForOSS     bool
	UpstreamAPI                        bool
	UpstreamAPITokenFile               string
	TLSPassthrough                     bool
	TLSPassthroughPort                 int
	VariablesHashBucketSize            uint64
	VariablesHashMaxSize               uint64
	UseForwardedHeaders                bool
	WorkerConnections                  string
	WorkerCPUAffinity                  string
	WorkerProcesses                    string
	WorkerRlimitNofile                 string
	WorkerShutdownTimeout              string
	AppProtectLoadModule               bool
	AppProtectV5LoadModule             bool
	AppProtectV5EnforcerAddr           string
	AppProtectFailureModeAction        string
	AppProtectCompressedRequestsAction string
	AppProtectCookieSeed               string
	AppProtectCPUThresholds            string
	AppProtectPhysicalMemoryThresholds string
	AppProtectReconnectPeriod          string
	AppProtectDosLoadModule            bool
	AppProtectDosLogFormat             []string
	AppProtectDosLogFormatEscaping     string
	AppProtectDosArbFqdn               string
	LatencyMetrics                     bool
	ZoneSyncConfig                     ZoneSyncConfig
	OIDC                               OIDCConfig
	DynamicSSLReloadEnabled            bool
	StaticSSLPath                      string
	NginxVersion                       nginx.Version
}

// NewUpstreamWithDefaultServer creates an upstream with the default server.
// proxy_pass to an upstream with the default server returns 502.
// We use it for services that have no endpoints.
func NewUpstreamWithDefaultServer(name string) Upstream {
	return Upstream{
		Name:             name,
		UpstreamZoneSize: "256k",
		UpstreamServers: []UpstreamServer{
			{
				Address:     "127.0.0.1:8181",
				MaxFails:    1,
				MaxConns:    0,
				FailTimeout: "10s",
			},
		},
	}
}

// IngressNginxCORS is what ingress-nginx's CORS annotations ask for. Every
// value is validated as ingress-nginx validates it before it gets here.
type IngressNginxCORS struct {
	// AnyOrigin answers every origin with Access-Control-Allow-Origin: *;
	// otherwise OriginRegex is what an Origin header must match (none if empty).
	AnyOrigin     bool
	OriginRegex   string
	Methods       string
	Headers       string
	Credentials   bool
	ExposeHeaders string
	MaxAge        int
}

// IngressNginxErrorLocation is a named location that sends an error to the backend of the errors, as ingress-nginx does.
type IngressNginxErrorLocation struct {
	Name     string
	Code     int // 0: the fallback of default-backend, which keeps the request
	Upstream string
}

// IngressNginxMirror is ingress-nginx's mirror of the requests of an Ingress.
type IngressNginxMirror struct {
	Source      string
	Target      string
	Host        string
	RequestBody string
}
