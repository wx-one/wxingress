package configs

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strings"

	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/gateway/extras"
	nl "github.com/wx-one/wxingress/internal/logger"
	"github.com/wx-one/wxingress/internal/nginx"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// GatewayDefaultServerConfigName is the config of the default server of the
// ports nginx listens on for Gateways with addresses of their own.
const GatewayDefaultServerConfigName = "_default-server-gateways"

// UpdateGatewayDefaultServer makes the default server of nginx's default
// ports the default server of the ports of the Gateways' listeners as well,
// in a config of its own: on a Gateway's port, a request for a hostname none
// of its listeners has gets the answer of the default server (404), not one
// of the routes of another hostname. The ports change only when listeners
// of Gateways come or go, which reloads nginx.
func (cnf *Configurator) UpdateGatewayDefaultServer(httpPorts, httpsPorts []int) error {
	if slices.Equal(cnf.gatewayHTTPPorts, httpPorts) && slices.Equal(cnf.gatewayHTTPSPorts, httpsPorts) {
		return nil
	}
	cnf.gatewayHTTPPorts = slices.Clone(httpPorts)
	cnf.gatewayHTTPSPorts = slices.Clone(httpsPorts)
	changed, err := cnf.writeGatewayDefaultServer()
	if err != nil || !changed {
		return err
	}
	return cnf.Reload(nginx.ReloadForOtherUpdate)
}

// writeGatewayDefaultServer writes the default server of the Gateways'
// ports, or removes it when there are none. It tells whether nginx needs a
// reload for it.
func (cnf *Configurator) writeGatewayDefaultServer() (bool, error) {
	if len(cnf.gatewayHTTPPorts) == 0 && len(cnf.gatewayHTTPSPorts) == 0 {
		if !cnf.gatewayDefaultServerWritten {
			return false, nil
		}
		cnf.nginxManager.DeleteConfig(GatewayDefaultServerConfigName)
		cnf.forgetConfig(GatewayDefaultServerConfigName)
		cnf.gatewayDefaultServerWritten = false
		return true, nil
	}

	cfg := cnf.buildDefaultServerConfig()
	server := &cfg.Servers[0]
	server.Ports = slices.Clone(cnf.gatewayHTTPPorts)
	server.SSLPorts = slices.Clone(cnf.gatewayHTTPSPorts)
	server.SSL = len(cnf.gatewayHTTPSPorts) > 0
	// the health check and TLS passthrough are the default ports' alone
	server.HealthStatus = false
	server.TLSPassthrough = false
	// the Gateways' servers speak HTTP/2, h2c too, and on a port without TLS
	// it is the default server's that counts
	server.HTTP2 = true
	content, err := cnf.templateExecutor.ExecuteIngressConfigTemplate(&cfg)
	if err != nil {
		return false, fmt.Errorf("error generating the default server of the Gateways' ports: %w", err)
	}
	changed, err := cnf.nginxManager.CreateConfig(GatewayDefaultServerConfigName, content)
	if err != nil {
		return false, fmt.Errorf("error writing the default server of the Gateways' ports: %w", err)
	}
	cnf.gatewayDefaultServerWritten = true
	return cnf.configChanged(GatewayDefaultServerConfigName, content, changed), nil
}

// gatewayNamePrefix starts the names of the VirtualServers made from
// Gateways (internal/gateway.NamePrefix), which no VirtualServer from the API
// can have.
const gatewayNamePrefix = "GW-"

// applyGatewayExtras applies to the config of a VirtualServer made from a
// Gateway what the Gateway API's filters need beyond what a VirtualServer
// says (internal/gateway/extras). A VirtualServer from the API never has a
// name a Gateway's can have, so its annotations are never read here.
func (cnf *Configurator) applyGatewayExtras(vs *conf_v1.VirtualServer, cfg *version2.VirtualServerConfig, warnings Warnings) {
	if !strings.HasPrefix(vs.Name, gatewayNamePrefix) {
		return
	}
	namer := NewUpstreamNamerForVirtualServer(vs)
	if err := extras.Apply(vs.Annotations, cfg, namer.GetNameForUpstream, canaryVariable); err != nil {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Errorf(l, "The filters of the Gateway of VirtualServer %s/%s are not applied: %v", vs.Namespace, vs.Name, err)
		warnings.AddWarningf(vs, "the filters of the Gateway are not applied: %v", err)
	}
}

// applyGatewayStreamExtras is applyGatewayExtras for a TransportServer made
// from a Gateway: ssl_preread and the default server of a TLS passthrough
// port, split_clients for weighted TCP and UDP backends.
func (cnf *Configurator) applyGatewayStreamExtras(ts *conf_v1.TransportServer, cfg *version2.TransportServerConfig, warnings Warnings) {
	if !strings.HasPrefix(ts.Name, gatewayNamePrefix) {
		return
	}
	namer := newUpstreamNamerForTransportServer(ts)
	if err := extras.ApplyStream(ts.Annotations, cfg, namer.GetNameForUpstream); err != nil {
		l := nl.LoggerFromContext(cnf.CfgParams.Context)
		nl.Errorf(l, "The extras of the Gateway of TransportServer %s/%s are not applied: %v", ts.Namespace, ts.Name, err)
		warnings.AddWarningf(ts, "the extras of the Gateway are not applied: %v", err)
	}
}

// GatewaySettings are what the ingress-nginx annotations of an HTTPRoute or
// GRPCRoute say about the backends of its rules - the same annotations as on
// an Ingress, read by the same parsers - as the fields of a VirtualServer
// upstream, and with -dynamic-upstreams the rule of the wxingress module's
// balancer: ewma, round_robin, chash, chashsubset and the sticky ones of
// affinity, as for an Ingress.
type GatewaySettings struct {
	// Upstream has the fields the annotations set; the rest are zero.
	Upstream conf_v1.Upstream
	// Balancing tells that the annotations ask for a balancer of the module.
	Balancing bool
	// Warnings are about the annotations that are ignored.
	Warnings []string

	// CanaryBy is the header and cookie part of a canary rule of the module
	// (canary-by-header, -header-value, -header-pattern, canary-by-cookie)
	// for a rule of the route with two backends; its weights are the
	// backends'.
	CanaryBy *nginx.Canary
	// ServerSnippets and LocationSnippets are the route's snippets
	// (nginx.org/server-snippets and nginx.org/location-snippets, or
	// ingress-nginx's server-snippet and configuration-snippet), only with
	// -enable-snippets.
	ServerSnippets   []string
	LocationSnippets []string
	// Restricted are the annotations of the route that restrict who gets an
	// answer - auth-*, *list-source-range, satisfy, ModSecurity, global rate
	// limits - which a route does not take: its rules are not served without
	// them but answer 503, as an Ingress location whose restriction cannot
	// apply.
	Restricted []string

	affinity       *ingressNginxAffinity
	balancing      *ingressNginxBalancing
	rateLimit      *version1.IngressNginxRateLimit
	limitRate      string
	limitRateAfter string
}

// IngressNginxBackendProtocolAnnotation is ingress-nginx's protocol of the backends.
const IngressNginxBackendProtocolAnnotation = IngressNginxAnnotationPrefix + "backend-protocol"

// canaryByAnnotations are the annotations of a canary rule a route takes;
// the weights are those of its backends.
var canaryByAnnotations = []string{CanaryByHeaderAnnotation, CanaryByHeaderValueAnnotation, CanaryByHeaderPatternAnnotation, CanaryByCookieAnnotation}

// ParseGatewaySettings reads the ingress-nginx annotations of a route, and
// its nginx.org snippets.
func ParseGatewaySettings(namespace, name string, annotations map[string]string, enableSnippets bool) GatewaySettings {
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Annotations: annotations}}
	a := newIngressNginxAnnotations(ing)
	cfg := &ConfigParams{}
	parseIngressNginxProxyAnnotations(a, cfg, false)
	cfg.IngressNginxAffinity = parseIngressNginxAffinity(a)
	parseIngressNginxLoadBalancing(a, cfg)
	parseIngressNginxRateLimitAnnotations(a, &IngressEx{Ingress: ing}, cfg)
	serverSnippets, locationSnippets := parseIngressNginxSnippets(a, enableSnippets)
	// the snippets of NGINX Ingress Controller, which an Ingress with them
	// but without -enable-snippets is invalid for; a route is served without them
	for annotation, into := range map[string]*[]string{"nginx.org/server-snippets": &serverSnippets, "nginx.org/location-snippets": &locationSnippets} {
		if snippets, exists := GetMapKeyAsStringSlice(annotations, annotation, ing, "\n"); exists {
			if !enableSnippets {
				a.warnings.AddWarningf(ing, "%s is ignored: snippets are not enabled", annotation)
				continue
			}
			*into = snippets
		}
	}

	u := conf_v1.Upstream{
		ProxyConnectTimeout:  cfg.ProxyConnectTimeout,
		ProxyReadTimeout:     cfg.ProxyReadTimeout,
		ProxySendTimeout:     cfg.ProxySendTimeout,
		ClientMaxBodySize:    cfg.ClientMaxBodySize,
		ClientBodyBufferSize: cfg.ClientBodyBufferSize,
		ProxyBufferSize:      cfg.ProxyBufferSize,
		ProxyBusyBuffersSize: cfg.ProxyBusyBuffersSize,
		ProxyHTTPVersion:     cfg.ProxyHTTPVersion,
		ProxyNextUpstream:    cfg.ProxyNextUpstream,
		LBMethod:             cfg.LBMethod,
		UseClusterIP:         UsesClusterIP(ing),
	}
	if cfg.ProxyNextUpstreamTimeout != "" {
		u.ProxyNextUpstreamTimeout = cfg.ProxyNextUpstreamTimeout + "s"
	}
	if cfg.ProxyNextUpstreamTries != nil {
		u.ProxyNextUpstreamTries = int(*cfg.ProxyNextUpstreamTries) //nolint:gosec // a small count
	}
	// backend-protocol HTTPS or GRPCS: TLS to the backends, which an
	// egressMTLS Policy then gives a client certificate and verification,
	// as a VirtualServer upstream with tls.enable
	if protocol, exists := a.value(IngressNginxBackendProtocolAnnotation); exists {
		switch strings.ToUpper(protocol) {
		case "HTTPS", "GRPCS":
			u.TLS.Enable = true
		case "HTTP", "GRPC", "AUTO_HTTP":
		default:
			a.invalid(IngressNginxBackendProtocolAnnotation, protocol, "a route takes HTTP, HTTPS, GRPC, GRPCS or AUTO_HTTP")
		}
	}
	if _, set := annotations[IngressNginxProxyBufferingAnnotation]; set {
		buffering := cfg.ProxyBuffering
		u.ProxyBuffering = &buffering
	}
	if n, size := splitProxyBuffers(cfg.ProxyBuffers); n > 0 {
		u.ProxyBuffers = &conf_v1.UpstreamBuffers{Number: n, Size: size}
	}
	// what ewma and round_robin are without the module, as for an Ingress;
	// least_time is NGINX Plus', which a VirtualServer upstream does not take
	if strings.HasPrefix(u.LBMethod, "least_time") {
		u.LBMethod = ""
	}

	s := GatewaySettings{
		Upstream: u, ServerSnippets: serverSnippets, LocationSnippets: locationSnippets,
		affinity: cfg.IngressNginxAffinity, balancing: cfg.IngressNginxBalancing,
		rateLimit: cfg.IngressNginxRateLimit, limitRate: cfg.IngressNginxLimitRate, limitRateAfter: cfg.IngressNginxLimitRateAfter,
	}
	_, s.Balancing = s.Balancer("x", nil)
	for _, annotation := range canaryByAnnotations {
		if _, set := annotations[annotation]; !set {
			continue
		}
		canary, err := ParseCanaryAnnotations(ing)
		if err != nil {
			a.warnings.AddWarningf(ing, "the canary annotations are ignored: %v", err)
			break
		}
		canary.Weight, canary.WeightTotal = 0, 0
		s.CanaryBy = &canary
		break
	}
	s.Restricted = flagUnsupportedOnRoutes(a)
	for _, w := range a.warnings {
		s.Warnings = append(s.Warnings, w...)
	}
	return s
}

// ingressNginxReadOnRoutes are the ingress-nginx annotations a route takes
// (docs/user-guide/ingress-nginx-compatibility.md, "The Gateway API").
var ingressNginxReadOnRoutes = map[string]bool{}

func init() {
	for _, name := range []string{
		"proxy-connect-timeout", "proxy-read-timeout", "proxy-send-timeout", "proxy-body-size", "client-body-buffer-size",
		"proxy-buffering", "proxy-request-buffering", "proxy-buffer-size", "proxy-buffers-number", "proxy-busy-buffers-size",
		"proxy-max-temp-file-size", "proxy-http-version", "proxy-next-upstream", "proxy-next-upstream-timeout",
		"proxy-next-upstream-tries", "service-upstream", "backend-protocol",
		"affinity", "affinity-mode", "affinity-canary-behavior", "session-cookie-change-on-failure",
		"session-cookie-conditional-samesite-none", "session-cookie-domain", "session-cookie-expires",
		"session-cookie-max-age", "session-cookie-name", "session-cookie-path", "session-cookie-samesite", "session-cookie-secure",
		"load-balance", "upstream-hash-by", "upstream-hash-by-subset", "upstream-hash-by-subset-size",
		"limit-rps", "limit-rpm", "limit-connections", "limit-burst-multiplier", "limit-allowlist", "limit-whitelist",
		"limit-rate", "limit-rate-after", "server-snippet", "configuration-snippet",
		"canary-by-header", "canary-by-header-value", "canary-by-header-pattern", "canary-by-cookie",
	} {
		ingressNginxReadOnRoutes[IngressNginxAnnotationPrefix+name] = true
	}
}

// ingressNginxRestrictive are those that restrict who gets an answer: the
// rules of a route with one answer 503 rather than be served without the
// restriction.
var ingressNginxRestrictive = regexp.MustCompile(`^nginx\.ingress\.kubernetes\.io/(auth-.*|(allow|white|deny)list-source-range|enable-modsecurity|enable-owasp-core-rules|modsecurity-snippet|satisfy|global-rate-limit.*|enable-global-auth)$`)

// flagUnsupportedOnRoutes warns about each ingress-nginx annotation of a
// route it does not take, and returns those that restrict who gets an answer.
func flagUnsupportedOnRoutes(a *ingressNginxAnnotations) []string {
	var names []string
	for name := range a.ing.Annotations {
		if strings.HasPrefix(name, IngressNginxAnnotationPrefix) && !ingressNginxReadOnRoutes[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var restricted []string
	for _, name := range names {
		if ingressNginxRestrictive.MatchString(name) && !disablesOnly(name, a.ing.Annotations[name]) {
			a.warnings.AddWarningf(a.ing, "%s is not supported on Gateway API routes: the rules of the route answer 503 - use a Policy (wxingress.wizardtales.com/policies)", name)
			restricted = append(restricted, name)
			continue
		}
		a.warnings.AddWarningf(a.ing, "%s is not supported on Gateway API routes and is ignored", name)
	}
	return restricted
}

// disablesOnly tells whether a restrictive annotation restricts nothing, as
// on an Ingress: one that turns its restriction off, the global auth that
// wxingress does not have, and the parts of a global rate limit without one.
func disablesOnly(name, value string) bool {
	value = strings.TrimSpace(value)
	switch short := strings.TrimPrefix(name, IngressNginxAnnotationPrefix); {
	case short == "enable-modsecurity", short == "enable-owasp-core-rules":
		return strings.EqualFold(value, "false")
	case short == "enable-global-auth":
		return true
	case short == "global-rate-limit":
		return value == "0"
	case strings.HasPrefix(short, "global-rate-limit-"):
		return true
	}
	return false
}

// Empty tells whether the annotations set nothing.
func (s GatewaySettings) Empty() bool {
	return reflect.DeepEqual(s.Upstream, conf_v1.Upstream{}) && !s.Balancing && s.CanaryBy == nil &&
		len(s.ServerSnippets) == 0 && len(s.LocationSnippets) == 0 && !s.Limits() && len(s.Restricted) == 0
}

// Limits tells whether the annotations limit requests, connections or the rate.
func (s GatewaySettings) Limits() bool {
	return s.rateLimit != nil || s.limitRate != "" || s.limitRateAfter != ""
}

// RateLimit is the config of limit-rps, limit-rpm, limit-connections,
// limit-rate and limit-rate-after, as an Ingress renders it: the http text
// with the zones of the limit, named after scope - unique among all configs -
// and the directives of the locations of the route.
func (s GatewaySettings) RateLimit(scope string) (http string, directives []string) {
	if rl := s.rateLimit; rl != nil {
		named := *rl
		named.Name = scope
		named.ID = ingressNginxLimitVariableReplacer.Replace(strings.ReplaceAll(base64.URLEncoding.EncodeToString([]byte(scope)), "=", ""))
		http = named.HTTP()
		directives = named.Directives()
	}
	if s.limitRateAfter != "" {
		directives = append(directives, "limit_rate_after "+s.limitRateAfter+";")
	}
	if s.limitRate != "" {
		directives = append(directives, "limit_rate "+s.limitRate+";")
	}
	return http, directives
}

// Balancer is the module's balancer rule for the backend - ingress-nginx's
// name of it, <namespace>-<service>-<port> - served at the paths, as
// ingress-nginx's get_implementation picks it: the affinity, else
// upstream-hash-by, else load-balance.
func (s GatewaySettings) Balancer(backend string, paths []string) (nginx.Balancer, bool) {
	if s.affinity != nil {
		return s.affinity.balancer(backend, paths), true
	}
	return s.balancing.balancer(backend)
}

// gatewayBalancers are the balancer rules a VirtualServer made from a Gateway
// has in its extras, by the name of their nginx upstream.
func gatewayBalancers(vs *conf_v1.VirtualServer) map[string]nginx.Balancer {
	if !strings.HasPrefix(vs.Name, gatewayNamePrefix) {
		return nil
	}
	e, err := extras.Decode(vs.Annotations[extras.Annotation])
	if err != nil || len(e.Balancers) == 0 {
		return nil
	}
	namer := NewUpstreamNamerForVirtualServer(vs)
	out := make(map[string]nginx.Balancer, len(e.Balancers))
	for upstream, raw := range e.Balancers {
		var b nginx.Balancer
		if json.Unmarshal(raw, &b) == nil {
			out[namer.GetNameForUpstream(upstream)] = b
		}
	}
	return out
}

// gatewayCanaries are the canaries of the module a VirtualServer made from a
// Gateway has - a rule with two weighted backends - by the nginx name of the
// primary upstream, as an Ingress canary's.
func gatewayCanaries(vs *conf_v1.VirtualServer) map[string]nginx.Canary {
	if !strings.HasPrefix(vs.Name, gatewayNamePrefix) {
		return nil
	}
	e, err := extras.Decode(vs.Annotations[extras.Annotation])
	if err != nil {
		return nil
	}
	namer := NewUpstreamNamerForVirtualServer(vs)
	out := make(map[string]nginx.Canary)
	for _, a := range e.Actions {
		if a.Canary == nil {
			continue
		}
		out[namer.GetNameForUpstream(a.Canary.Primary)] = nginx.Canary{
			Canary:        namer.GetNameForUpstream(a.Canary.Upstream),
			Weight:        a.Canary.Weight,
			WeightTotal:   a.Canary.WeightTotal,
			Header:        a.Canary.Header,
			HeaderValue:   a.Canary.HeaderValue,
			HeaderPattern: a.Canary.HeaderPattern,
			Cookie:        a.Canary.Cookie,
		}
	}
	return out
}
