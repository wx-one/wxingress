package configs

import (
	"maps"
	"regexp"
	"sort"
	"strings"

	networking "k8s.io/api/networking/v1"
)

// Annotations of ingress-nginx for the server and the redirect to https, and
// those that mean what an nginx.org annotation means.
const (
	IngressNginxSSLRedirectAnnotation            = IngressNginxAnnotationPrefix + "ssl-redirect"
	IngressNginxForceSSLRedirectAnnotation       = IngressNginxAnnotationPrefix + "force-ssl-redirect"
	IngressNginxPreserveTrailingSlashAnnotation  = IngressNginxAnnotationPrefix + "preserve-trailing-slash"
	IngressNginxEnableAccessLogAnnotation        = IngressNginxAnnotationPrefix + "enable-access-log"
	IngressNginxServerAliasAnnotation            = IngressNginxAnnotationPrefix + "server-alias"
	IngressNginxUpstreamVhostAnnotation          = IngressNginxAnnotationPrefix + "upstream-vhost"
	IngressNginxAppRootAnnotation                = IngressNginxAnnotationPrefix + "app-root"
	IngressNginxSSLCiphersAnnotation             = IngressNginxAnnotationPrefix + "ssl-ciphers"
	IngressNginxSSLPreferServerCiphersAnnotation = IngressNginxAnnotationPrefix + "ssl-prefer-server-ciphers"
	IngressNginxProxyRedirectFromAnnotation      = IngressNginxAnnotationPrefix + "proxy-redirect-from"
	IngressNginxProxyRedirectToAnnotation        = IngressNginxAnnotationPrefix + "proxy-redirect-to"
	IngressNginxFromToWWWRedirectAnnotation      = IngressNginxAnnotationPrefix + "from-to-www-redirect"
)

// IngressNginxFromToWWW tells whether the Ingress asks for ingress-nginx's from-to-www-redirect.
func IngressNginxFromToWWW(ing *networking.Ingress) bool {
	return strings.TrimSpace(ing.Annotations[IngressNginxFromToWWWRedirectAnnotation]) == "true"
}

func init() {
	for _, name := range []string{
		IngressNginxSSLRedirectAnnotation, IngressNginxForceSSLRedirectAnnotation,
		IngressNginxPreserveTrailingSlashAnnotation, IngressNginxEnableAccessLogAnnotation,
		IngressNginxServerAliasAnnotation, IngressNginxUpstreamVhostAnnotation, IngressNginxAppRootAnnotation,
		IngressNginxSSLCiphersAnnotation, IngressNginxSSLPreferServerCiphersAnnotation,
		IngressNginxProxyRedirectFromAnnotation, IngressNginxProxyRedirectToAnnotation,
		IngressNginxFromToWWWRedirectAnnotation,
	} {
		ingressNginxRead[name] = true
	}
}

var (
	// what a value may be that goes into the nginx configuration as it is
	ingressNginxVhostRegexp        = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]_$]+$`)
	ingressNginxAppRootRegexp      = regexp.MustCompile(`^/[A-Za-z0-9\-._~/%!$&'()*+,=:@]*$`)
	ingressNginxCiphersRegexp      = regexp.MustCompile(`^[A-Za-z0-9!:+\-_@=.]+$`)
	ingressNginxProxyRedirectRegex = regexp.MustCompile(`^[^\s;{}"'\\]+$`)
	ingressNginxServerAliasRegexp  = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
)

// ingressNginxAliases are the annotations of ingress-nginx that mean what an
// nginx.org one means, with what their value must look like.
var ingressNginxAliases = []struct {
	from, to string
	valid    *regexp.Regexp
}{
	{IngressNginxUpstreamVhostAnnotation, UpstreamVhostAnnotation, ingressNginxVhostRegexp},
	{IngressNginxAppRootAnnotation, "nginx.org/app-root", ingressNginxAppRootRegexp},
	{IngressNginxSSLCiphersAnnotation, SSLCiphersAnnotation, ingressNginxCiphersRegexp},
	{IngressNginxSSLPreferServerCiphersAnnotation, "nginx.org/ssl-prefer-server-ciphers", regexp.MustCompile(`^(true|false)$`)},
	{IngressNginxProxyRedirectFromAnnotation, ProxyRedirectFromAnnotation, ingressNginxProxyRedirectRegex},
	{IngressNginxProxyRedirectToAnnotation, "nginx.org/proxy-redirect-to", ingressNginxProxyRedirectRegex},
	{IngressNginxSSLRedirectAnnotation, SSLRedirectAnnotation, regexp.MustCompile(`^(true|false)$`)},
}

// withIngressNginxAliases returns ingEx with an Ingress whose annotations
// have, for each ingress-nginx annotation that means what an nginx.org one
// means, that nginx.org annotation - unless it is set, which wins, as
// everywhere. backend-protocol becomes nginx.org/ssl-services and
// nginx.org/grpc-services for every Service of the Ingress. An invalid value
// is left out with a warning, as ingress-nginx ignores an invalid annotation.
func withIngressNginxAliases(ingEx *IngressEx) (*IngressEx, Warnings) {
	warnings := newWarnings()
	ing := ingEx.Ingress
	var added map[string]string
	add := func(name, value string) {
		if _, set := ing.Annotations[name]; set {
			return
		}
		if added == nil {
			added = map[string]string{}
		}
		added[name] = value
	}
	for _, alias := range ingressNginxAliases {
		value, exists := ing.Annotations[alias.from]
		if !exists {
			continue
		}
		value = strings.TrimSpace(value)
		if _, set := ing.Annotations[alias.to]; set {
			warnings.AddWarningf(ing, "%s is ignored: %s is set and takes precedence", alias.from, alias.to)
			continue
		}
		if !alias.valid.MatchString(value) {
			warnings.AddWarningf(ing, "%s is ignored: %q is invalid", alias.from, value)
			continue
		}
		add(alias.to, value)
	}
	if protocol, exists := ing.Annotations[IngressNginxBackendProtocolAnnotation]; exists {
		services := strings.Join(ingressServiceNames(ing), ",")
		switch strings.ToUpper(strings.TrimSpace(protocol)) {
		case "HTTPS":
			add("nginx.org/ssl-services", services)
		case "GRPC":
			add("nginx.org/grpc-services", services)
		case "GRPCS":
			add("nginx.org/ssl-services", services)
			add("nginx.org/grpc-services", services)
		case "HTTP", "AUTO_HTTP", "FCGI":
			// FCGI: parseIngressNginxFastCGI
		default:
			// AJP and the like: serving it as HTTP would send the backend what it does not speak
			warnings.AddWarningf(ing, "the locations of the Ingress answer %d: %s %q is not supported by wxingress", ingressNginxDeniedCode, IngressNginxBackendProtocolAnnotation, protocol)
			add(ingressNginxDeniedAnnotation, "true")
		}
	}
	if added == nil {
		return ingEx, warnings
	}
	copied := *ing
	copied.Annotations = maps.Clone(ing.Annotations)
	maps.Copy(copied.Annotations, added)
	ex := *ingEx
	ex.Ingress = &copied
	return &ex, warnings
}

// ingressNginxDeniedAnnotation marks, on the copy withIngressNginxAliases makes, an Ingress
// whose locations answer 503; it never comes from a user, whose annotations it is not in.
const ingressNginxDeniedAnnotation = "wxingress.internal/ingress-nginx-denied"

// ingressServiceNames are the Services the Ingress names, sorted.
func ingressServiceNames(ing *networking.Ingress) []string {
	seen := map[string]bool{}
	if b := ing.Spec.DefaultBackend; b != nil && b.Service != nil {
		seen[b.Service.Name] = true
	}
	for _, r := range ing.Spec.Rules {
		if r.HTTP == nil {
			continue
		}
		for _, p := range r.HTTP.Paths {
			if p.Backend.Service != nil {
				seen[p.Backend.Service.Name] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// parseIngressNginxServerAnnotations reads force-ssl-redirect,
// preserve-trailing-slash, enable-access-log and server-alias.
func parseIngressNginxServerAnnotations(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	if a.ing.Annotations[ingressNginxDeniedAnnotation] == "true" {
		cfgParams.IngressNginxDenied = true
	}
	if force, ok := a.boolean(IngressNginxForceSSLRedirectAnnotation); ok {
		cfgParams.IngressNginxForceSSLRedirect = force
	}
	if preserve, ok := a.boolean(IngressNginxPreserveTrailingSlashAnnotation); ok {
		cfgParams.IngressNginxPreserveTrailingSlash = preserve
	}
	if enabled, ok := a.boolean(IngressNginxEnableAccessLogAnnotation); ok {
		cfgParams.IngressNginxAccessLogOff = !enabled
	}
	if value, ok := a.value(IngressNginxServerAliasAnnotation); ok {
		var aliases []string
		for _, alias := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
			if !ingressNginxServerAliasRegexp.MatchString(alias) {
				a.invalid(IngressNginxServerAliasAnnotation, alias, "it must be a host name")
				continue
			}
			aliases = append(aliases, alias)
		}
		cfgParams.IngressNginxServerAliases = aliases
	}
}
