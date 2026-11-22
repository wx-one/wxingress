package configs

import (
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
)

// More annotations of ingress-nginx: the Connection header to the backend,
// response headers and headers to the auth service from ConfigMaps, the
// keepalive of the auth service, tracing and stream snippets.
const (
	IngressNginxConnectionProxyHeaderAnnotation   = IngressNginxAnnotationPrefix + "connection-proxy-header"
	IngressNginxCustomHeadersAnnotation           = IngressNginxAnnotationPrefix + "custom-headers"
	IngressNginxAuthKeepaliveRequestsAnnotation   = IngressNginxAnnotationPrefix + "auth-keepalive-requests"
	IngressNginxAuthKeepaliveTimeoutAnnotation    = IngressNginxAnnotationPrefix + "auth-keepalive-timeout"
	IngressNginxAuthKeepaliveShareVarsAnnotation  = IngressNginxAnnotationPrefix + "auth-keepalive-share-vars"
	IngressNginxEnableOpentelemetryAnnotation     = IngressNginxAnnotationPrefix + "enable-opentelemetry"
	IngressNginxOtelTrustIncomingSpanAnnotation   = IngressNginxAnnotationPrefix + "opentelemetry-trust-incoming-span"
	IngressNginxOtelOperationNameAnnotation       = IngressNginxAnnotationPrefix + "opentelemetry-operation-name"
	IngressNginxStreamSnippetAnnotation           = IngressNginxAnnotationPrefix + "stream-snippet"
	IngressNginxFastCGIIndexAnnotation            = IngressNginxAnnotationPrefix + "fastcgi-index"
	IngressNginxFastCGIParamsAnnotation           = IngressNginxAnnotationPrefix + "fastcgi-params-configmap"
	ingressNginxDefaultAuthKeepaliveRequests      = 1000
	ingressNginxDefaultAuthKeepaliveTimeout       = 60
	ingressNginxAuthUpstreamPrefix                = "ingress-nginx-auth"
	ingressNginxDefaultOtelTrustIncomingSpanValue = true
)

func init() {
	for _, name := range []string{
		IngressNginxConnectionProxyHeaderAnnotation, IngressNginxCustomHeadersAnnotation,
		IngressNginxAuthKeepaliveRequestsAnnotation, IngressNginxAuthKeepaliveTimeoutAnnotation,
		IngressNginxAuthKeepaliveShareVarsAnnotation, IngressNginxEnableOpentelemetryAnnotation,
		IngressNginxOtelTrustIncomingSpanAnnotation, IngressNginxOtelOperationNameAnnotation,
		IngressNginxStreamSnippetAnnotation, IngressNginxFastCGIIndexAnnotation, IngressNginxFastCGIParamsAnnotation,
	} {
		ingressNginxRead[name] = true
	}
}

var (
	// ingress-nginx's validators of connection-proxy-header, of the names and
	// values of the headers in the ConfigMaps of custom-headers and
	// auth-proxy-set-headers, and of opentelemetry-operation-name
	ingressNginxConnectionRegexp    = regexp.MustCompile(`^(close|keep-alive)$`)
	ingressNginxHeaderNameRegexp    = regexp.MustCompile(`^[a-zA-Z\d\-_]+$`)
	ingressNginxHeaderValueRegexp   = regexp.MustCompile("^[a-zA-Z\\d_ :;.,\\\\/\"'?!(){}\\[\\]@<>=\\-+*#$&\\x60|~^%]+$")
	ingressNginxOperationNameRegexp = regexp.MustCompile(`^[A-Za-z0-9_\-]*$`)
	ingressNginxFastCGIKeyRegexp    = regexp.MustCompile(`^[A-Za-z0-9.\-_]+$`)
	ingressNginxFastCGIValueRegexp  = regexp.MustCompile(`^[A-Za-z0-9\-_$\{\}/.]*$`)
)

// IngressNginxConfigMapRefs are the ConfigMaps the Ingress names in
// custom-headers and auth-proxy-set-headers, as namespace/name. A name
// without a namespace is one of the Ingress's namespace; one of another
// namespace is listed too - the Ingress is denied for it (ingressNginxConfigMap).
func IngressNginxConfigMapRefs(ing *networking.Ingress) []string {
	var refs []string
	for _, name := range []string{IngressNginxCustomHeadersAnnotation, IngressNginxAuthProxySetHeadersAnnotation, IngressNginxFastCGIParamsAnnotation} {
		if key, ok := ingressNginxConfigMapKey(ing, ing.Annotations[name]); ok && ing.Namespace == strings.SplitN(key, "/", 2)[0] && !slices.Contains(refs, key) {
			refs = append(refs, key)
		}
	}
	return refs
}

func ingressNginxConfigMapKey(ing *networking.Ingress, value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	ns, name, found := strings.Cut(value, "/")
	if !found {
		ns, name = ing.Namespace, value
	}
	if ns == "" || name == "" || strings.Contains(name, "/") {
		return "", false
	}
	return ns + "/" + name, true
}

// ingressNginxConfigMap is the data of the ConfigMap the annotation names,
// with the headers it has checked: a name and a value ingress-nginx takes.
// Like ingress-nginx, it denies the locations of the Ingress (503) where it
// cannot take the ConfigMap: missing, a header it would not take - and,
// unlike ingress-nginx, one of another namespace (wxingress takes no
// resources of other namespaces, as allow-cross-namespace-resources: false).
func ingressNginxConfigMap(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams, name string, checkValues bool) (map[string]string, bool) {
	value, exists := a.value(name)
	if !exists {
		return nil, false
	}
	deny := func(format string, args ...interface{}) (map[string]string, bool) {
		args = append([]interface{}{ingressNginxDeniedCode, name}, args...)
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s: "+format, args...)
		cfgParams.IngressNginxDenied = true
		return nil, false
	}
	key, ok := ingressNginxConfigMapKey(a.ing, value)
	if !ok {
		return deny("%q is no ConfigMap", value)
	}
	if strings.SplitN(key, "/", 2)[0] != a.ing.Namespace {
		return deny("the ConfigMap %s is in another namespace than the Ingress", key)
	}
	cm := ingEx.IngressNginxConfigMaps[key]
	if cm == nil {
		return deny("the ConfigMap %s does not exist", key)
	}
	for header, v := range cm.Data {
		if !ingressNginxHeaderNameRegexp.MatchString(header) {
			return deny("the ConfigMap %s has the invalid header name %q", key, header)
		}
		if checkValues && !ingressNginxHeaderValueRegexp.MatchString(v) {
			return deny("the ConfigMap %s has an invalid value of %s", key, header)
		}
	}
	return cm.Data, true
}

// sortedHeaders are the headers of a ConfigMap sorted by name, a "$" of a
// value escaped as ingress-nginx does when escape.
func sortedHeaders(data map[string]string, escape bool) []version2.Header {
	headers := make([]version2.Header, 0, len(data))
	for _, name := range sortedKeys(data) {
		value := data[name]
		if escape {
			value = strings.ReplaceAll(value, "$", "${literal_dollar}")
		}
		headers = append(headers, version2.Header{Name: name, Value: value})
	}
	return headers
}

// parseIngressNginxFastCGI reads backend-protocol FCGI with fastcgi-index
// and fastcgi-params-configmap: the locations pass to their upstreams with
// FastCGI, nginx's fastcgi_params and the parameters of the ConfigMap, as
// in ingress-nginx. A ConfigMap that is missing, of another namespace or with
// what ingress-nginx does not take denies the locations (503).
func parseIngressNginxFastCGI(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	if !strings.EqualFold(strings.TrimSpace(a.ing.Annotations[IngressNginxBackendProtocolAnnotation]), "FCGI") {
		for _, name := range []string{IngressNginxFastCGIIndexAnnotation, IngressNginxFastCGIParamsAnnotation} {
			if _, exists := a.ing.Annotations[name]; exists {
				a.warnings.AddWarningf(a.ing, "%s is ignored: %s is not FCGI", name, IngressNginxBackendProtocolAnnotation)
			}
		}
		return
	}
	fcgi := &version1.IngressNginxFastCGI{}
	if index, ok := a.matching(IngressNginxFastCGIIndexAnnotation, ingressNginxFastCGIKeyRegexp, "it must be a file name"); ok {
		fcgi.Index = index
	}
	if _, exists := a.ing.Annotations[IngressNginxFastCGIParamsAnnotation]; exists {
		data, ok := ingressNginxConfigMap(a, ingEx, cfgParams, IngressNginxFastCGIParamsAnnotation, false)
		if !ok {
			return
		}
		for _, k := range sortedKeys(data) {
			v := data[k]
			if !ingressNginxFastCGIKeyRegexp.MatchString(k) || !ingressNginxFastCGIValueRegexp.MatchString(v) || checkIngressNginxVariables(v, false) != nil {
				a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s: the parameter %s is invalid", ingressNginxDeniedCode, IngressNginxFastCGIParamsAnnotation, k)
				cfgParams.IngressNginxDenied = true
				return
			}
		}
		fcgi.Params = sortedHeaders(data, false)
	}
	cfgParams.IngressNginxFastCGI = fcgi
}

// parseIngressNginxConnection reads connection-proxy-header, the value of
// the Connection header to the backend instead of the one the location
// would send.
func parseIngressNginxConnection(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	if v, ok := a.matching(IngressNginxConnectionProxyHeaderAnnotation, ingressNginxConnectionRegexp, "it must be close or keep-alive"); ok {
		cfgParams.IngressNginxConnection = v
	}
}

// parseIngressNginxCustomHeaders reads custom-headers: the headers of the
// ConfigMap are headers of every answer of the locations. As in
// ingress-nginx, each must be in the ConfigMap's
// global-allowed-response-headers, else the locations answer 503.
func parseIngressNginxCustomHeaders(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	data, ok := ingressNginxConfigMap(a, ingEx, cfgParams, IngressNginxCustomHeadersAnnotation, true)
	if !ok {
		return
	}
	for _, header := range sortedKeys(data) {
		if !slices.ContainsFunc(cfgParams.IngressNginxAllowedResponseHeaders, func(allowed string) bool { return strings.EqualFold(allowed, header) }) {
			a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s: the header %s is not in the ConfigMap's global-allowed-response-headers",
				ingressNginxDeniedCode, IngressNginxCustomHeadersAnnotation, header)
			cfgParams.IngressNginxDenied = true
			return
		}
	}
	cfgParams.IngressNginxCustomHeaders = sortedHeaders(data, true)
}

// parseIngressNginxOtel reads enable-opentelemetry,
// opentelemetry-trust-incoming-span and opentelemetry-operation-name into
// the otel_* directives of the locations. They need the otel module, which
// the ConfigMap's otel-exporter-endpoint (otlp-collector-host) loads: the
// traces go only to the collector it names.
func parseIngressNginxOtel(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	enabled, enableSet := a.boolean(IngressNginxEnableOpentelemetryAnnotation)
	trust, trustSet := a.boolean(IngressNginxOtelTrustIncomingSpanAnnotation)
	operation, operationSet := a.matching(IngressNginxOtelOperationNameAnnotation, ingressNginxOperationNameRegexp, "it may have letters, digits, _ and - only")
	if !enableSet && !trustSet && !operationSet {
		return
	}
	if !cfgParams.MainOtelLoadModule {
		a.warnings.AddWarningf(a.ing, "%s and the other opentelemetry annotations are ignored: the ConfigMap has no otel-exporter-endpoint", IngressNginxEnableOpentelemetryAnnotation)
		return
	}
	if !enableSet {
		enabled = cfgParams.MainOtelTraceInHTTP
	}
	if !enabled {
		if enableSet && cfgParams.MainOtelTraceInHTTP {
			cfgParams.IngressNginxOtel = &version1.IngressNginxOtel{}
		}
		return
	}
	otel := &version1.IngressNginxOtel{Trace: true, SpanName: operation}
	if !trustSet {
		trust = ingressNginxDefaultOtelTrustIncomingSpanValue
		if cfgParams.MainOtelTraceContext == "inject" || cfgParams.MainOtelTraceContext == "ignore" {
			trust = false
		}
	}
	otel.Context = "inject"
	if trust {
		otel.Context = "propagate"
	}
	cfgParams.IngressNginxOtel = otel
}

// parseIngressNginxStreamSnippet reads stream-snippet, configuration for
// the stream block. Like the other snippets it needs -enable-snippets.
func parseIngressNginxStreamSnippet(a *ingressNginxAnnotations, cfgParams *ConfigParams, enableSnippets bool) {
	value, exists := a.value(IngressNginxStreamSnippetAnnotation)
	if !exists {
		return
	}
	if !enableSnippets {
		a.warnings.AddWarningf(a.ing, "%s is ignored: snippets are not enabled", IngressNginxStreamSnippetAnnotation)
		return
	}
	cfgParams.IngressNginxStreamSnippet = strings.Split(value, "\n")
}

// parseIngressNginxAuthProxySetHeaders reads auth-proxy-set-headers, the
// ConfigMap of headers to the auth service. It tells whether the Ingress
// may be served.
func parseIngressNginxAuthProxySetHeaders(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams, auth *ingressNginxExternalAuth) bool {
	if _, exists := a.ing.Annotations[IngressNginxAuthProxySetHeadersAnnotation]; !exists {
		return true
	}
	data, ok := ingressNginxConfigMap(a, ingEx, cfgParams, IngressNginxAuthProxySetHeadersAnnotation, false)
	if !ok {
		return !cfgParams.IngressNginxDenied
	}
	// the values are those ingress-nginx takes of a header (no CR, LF or
	// other control character, which %q would make an escape nginx turns
	// back into a line break), empty to send none, with variables nginx knows
	for _, header := range sortedKeys(data) {
		v := data[header]
		if v != "" && !ingressNginxHeaderValueRegexp.MatchString(v) {
			a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s: the header %s has an invalid value",
				ingressNginxDeniedCode, IngressNginxAuthProxySetHeadersAnnotation, header)
			cfgParams.IngressNginxDenied = true
			return false
		}
		if err := checkIngressNginxVariables(v, true); err != nil {
			a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s: the header %s has an invalid value: %v",
				ingressNginxDeniedCode, IngressNginxAuthProxySetHeadersAnnotation, header, err)
			cfgParams.IngressNginxDenied = true
			return false
		}
	}
	// as ingress-nginx's proxy_set_header "name" "value": the value may have variables
	auth.ProxySetHeaders = sortedHeaders(data, false)
	return true
}

// parseIngressNginxAuthKeepalive reads auth-keepalive and the annotations
// that go with it: the auth service is an upstream with keepalive
// connections. As in ingress-nginx, not where the host of auth-url has a
// variable, and not with HTTP/2, where ingress-nginx does not keep them
// either. nginx keeps a connection of an auth request whose answer has no
// body (Content-Length: 0, as oauth2-proxy answers), not one with a body,
// which it does not read; auth-keepalive-share-vars is what nginx's
// subrequests do anyway.
func parseIngressNginxAuthKeepalive(a *ingressNginxAnnotations, cfgParams *ConfigParams, auth *ingressNginxExternalAuth) {
	connections, ok := a.intAtLeast(IngressNginxAuthKeepaliveAnnotation, 0)
	if !ok || connections == 0 {
		return
	}
	requests := ingressNginxDefaultAuthKeepaliveRequests
	if v, ok := a.intAtLeast(IngressNginxAuthKeepaliveRequestsAnnotation, 1); ok {
		requests = v
	}
	timeout := ingressNginxDefaultAuthKeepaliveTimeout
	if v, ok := a.intAtLeast(IngressNginxAuthKeepaliveTimeoutAnnotation, 1); ok {
		timeout = v
	}
	a.boolean(IngressNginxAuthKeepaliveShareVarsAnnotation)
	parsed, err := url.Parse(auth.URL)
	switch {
	case err != nil:
		return
	case strings.Contains(parsed.Host, "$"):
		a.warnings.AddWarningf(a.ing, "%s is ignored: the host of %s has a variable", IngressNginxAuthKeepaliveAnnotation, IngressNginxAuthURLAnnotation)
		return
	case cfgParams.HTTP2:
		a.warnings.AddWarningf(a.ing, "%s is ignored with HTTP/2, as in ingress-nginx", IngressNginxAuthKeepaliveAnnotation)
		return
	}
	port := parsed.Port()
	if port == "" {
		port = "80"
		if parsed.Scheme == "https" {
			port = "443"
		}
	}
	sum := sha256.Sum256([]byte(a.ing.Namespace + "/" + a.ing.Name + "|" + auth.URL))
	name := ingressNginxAuthUpstreamPrefix + "_" + a.ing.Namespace + "_" + a.ing.Name + "_" + hex.EncodeToString(sum[:])[:10]
	auth.upstream = &version1.IngressNginxAuthUpstream{
		Name:              name,
		Server:            net.JoinHostPort(parsed.Hostname(), port),
		Resolve:           net.ParseIP(parsed.Hostname()) == nil,
		Keepalive:         connections,
		KeepaliveRequests: requests,
		KeepaliveTimeout:  timeout,
	}
	// the URL goes to the upstream, with its path and query
	target := *parsed
	target.Host = name
	auth.Upstream = name
	auth.URL = target.String()
}

// ingressNginxAuthUpstreamsOf is the upstream of the auth service of an
// Ingress, if it keeps connections, with the resolver of its name.
func ingressNginxAuthUpstreamsOf(auth *ingressNginxExternalAuth, resolver string) []version1.IngressNginxAuthUpstream {
	if auth == nil || auth.upstream == nil {
		return nil
	}
	u := *auth.upstream
	if u.Resolve {
		u.Resolver = resolver
	}
	return []version1.IngressNginxAuthUpstream{u}
}

// applyIngressNginxMore puts what the annotations of this file read on the location.
func applyIngressNginxMore(loc *version1.Location, cfgParams *ConfigParams) {
	loc.IngressNginxConnection = cfgParams.IngressNginxConnection
	loc.IngressNginxOtel = cfgParams.IngressNginxOtel
	if !loc.GRPC {
		loc.IngressNginxFastCGI = cfgParams.IngressNginxFastCGI
	}
	if len(cfgParams.IngressNginxCustomHeaders) > 0 {
		loc.IngressNginxCustomHeaders = cfgParams.IngressNginxCustomHeaders
		if loc.AddHeaderInherit == "" {
			// the headers are added in the location; the server's stay
			loc.AddHeaderInherit = addHeaderInheritMerge
		}
	}
}

// uniqueStreamSnippets are the stream snippets of an Ingress and those merged into it, each once.
func uniqueStreamSnippets(snippets ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range snippets {
		joined := strings.Join(s, "\n")
		if joined == "" || seen[joined] {
			continue
		}
		seen[joined] = true
		out = append(out, joined)
	}
	return out
}

// uniqueAuthUpstreams are the auth upstreams of an Ingress and those merged into it, each once, sorted by name.
func uniqueAuthUpstreams(upstreams []version1.IngressNginxAuthUpstream) []version1.IngressNginxAuthUpstream {
	sort.SliceStable(upstreams, func(i, j int) bool { return upstreams[i].Name < upstreams[j].Name })
	return slices.CompactFunc(upstreams, func(a, b version1.IngressNginxAuthUpstream) bool { return a.Name == b.Name })
}
