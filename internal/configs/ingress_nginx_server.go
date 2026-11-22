package configs

import (
	"errors"
	"maps"
	"regexp"
	"sort"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/validation"
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
	// what the controller allows in nginx.org/proxy-redirect-from and -to
	ingressNginxProxyRedirectValueRegex = regexp.MustCompile("^[^;{}\x60\n\r\\s#]+$")
	ingressNginxServerAliasRegexp       = regexp.MustCompile(`^(\*\.)?[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$`)
)

// ingressNginxAliases are the annotations of ingress-nginx that mean what an
// nginx.org one means, with what their value must look like and, where it
// may have nginx variables or a regular expression, the check of what nginx
// -t would refuse: the controller validates the nginx.org annotations of the
// Ingress itself, not those an alias adds.
var ingressNginxAliases = []struct {
	from, to string
	valid    *regexp.Regexp
	check    func(string) error
}{
	{IngressNginxUpstreamVhostAnnotation, UpstreamVhostAnnotation, ingressNginxVhostRegexp, checkIngressNginxVariablesUnquoted},
	{IngressNginxAppRootAnnotation, "nginx.org/app-root", ingressNginxAppRootRegexp, checkIngressNginxVariablesUnquoted},
	{IngressNginxSSLCiphersAnnotation, SSLCiphersAnnotation, ingressNginxCiphersRegexp, nil},
	{IngressNginxSSLPreferServerCiphersAnnotation, "nginx.org/ssl-prefer-server-ciphers", regexp.MustCompile(`^(true|false)$`), nil},
	{IngressNginxProxyRedirectFromAnnotation, ProxyRedirectFromAnnotation, ingressNginxProxyRedirectRegex, checkIngressNginxProxyRedirectFrom},
	{IngressNginxProxyRedirectToAnnotation, "nginx.org/proxy-redirect-to", ingressNginxProxyRedirectRegex, checkIngressNginxProxyRedirectTo},
	{IngressNginxSSLRedirectAnnotation, SSLRedirectAnnotation, regexp.MustCompile(`^(true|false)$`), nil},
}

// checkIngressNginxVariablesUnquoted checks the variables of a value that
// goes into the configuration unquoted.
func checkIngressNginxVariablesUnquoted(value string) error {
	return checkIngressNginxVariables(value, false)
}

// checkIngressNginxProxyRedirectTo checks what the controller checks of
// nginx.org/proxy-redirect-to: no character that ends the directive, starts
// a comment or an argument, and only variables nginx knows.
func checkIngressNginxProxyRedirectTo(value string) error {
	if err := checkIngressNginxProxyRedirectValue(value); err != nil {
		return err
	}
	return checkIngressNginxVariables(value, false)
}

// checkIngressNginxProxyRedirectValue checks that the value does not end
// the directive, start a comment or another argument.
func checkIngressNginxProxyRedirectValue(value string) error {
	if !ingressNginxProxyRedirectValueRegex.MatchString(value) {
		return errors.New("it must not contain ';', '{', '}', '#', a backtick or whitespace")
	}
	return validation.ValidateDirectiveValue(value)
}

// checkIngressNginxProxyRedirectFrom checks what the controller checks of
// nginx.org/proxy-redirect-from: that of proxy-redirect-to or, where it is
// a regular expression (~ or ~*), which nginx compiles as it is, without
// variables, one PCRE compiles.
func checkIngressNginxProxyRedirectFrom(value string) error {
	if value == "off" || value == "default" {
		return nil
	}
	pattern, isRegex := strings.CutPrefix(value, "~*")
	if !isRegex {
		pattern, isRegex = strings.CutPrefix(value, "~")
	}
	if !isRegex {
		return checkIngressNginxProxyRedirectTo(value)
	}
	if err := checkIngressNginxProxyRedirectValue(value); err != nil {
		return err
	}
	// nginx unescapes the unquoted value before PCRE compiles it
	return CheckPCRERegex(validation.UnescapeNGINXToken(pattern))
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
		if alias.check != nil {
			if err := alias.check(value); err != nil {
				warnings.AddWarningf(ing, "%s is ignored: %q is invalid: %v", alias.from, value, err)
				continue
			}
		}
		add(alias.to, value)
	}
	dropIncompleteProxyRedirect(ing, added, warnings)
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

// dropIncompleteProxyRedirect leaves out the proxy-redirect-from or -to an
// alias added where the other one is missing, as the controller refuses
// nginx.org/proxy-redirect-to without -from and a -from other than off or
// default without -to: proxy_redirect would have one argument too few.
func dropIncompleteProxyRedirect(ing *networking.Ingress, added map[string]string, warnings Warnings) {
	lookup := func(name string) (string, bool) {
		if v, ok := added[name]; ok {
			return v, true
		}
		v, ok := ing.Annotations[name]
		return v, ok
	}
	from, hasFrom := lookup(ProxyRedirectFromAnnotation)
	_, hasTo := lookup(ProxyRedirectToAnnotation)
	if _, ok := added[ProxyRedirectToAnnotation]; ok && !hasFrom {
		warnings.AddWarningf(ing, "%s is ignored: it needs %s", IngressNginxProxyRedirectToAnnotation, IngressNginxProxyRedirectFromAnnotation)
		delete(added, ProxyRedirectToAnnotation)
	}
	if _, ok := added[ProxyRedirectFromAnnotation]; ok && !hasTo && from != "off" && from != "default" {
		warnings.AddWarningf(ing, "%s is ignored: it needs %s", IngressNginxProxyRedirectFromAnnotation, IngressNginxProxyRedirectToAnnotation)
		delete(added, ProxyRedirectFromAnnotation)
	}
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
		aliases, invalid := splitIngressNginxServerAliases(value)
		for _, alias := range invalid {
			a.invalid(IngressNginxServerAliasAnnotation, alias, "it must be a host name, a wildcard only of the form *.example.com")
		}
		cfgParams.IngressNginxServerAliases = aliases
	}
}

// splitIngressNginxServerAliases splits server-alias into the names the
// controller takes and those it does not. A wildcard must stand for one
// label and leave at least two: *.example.com, not *.com, which would take
// the requests of every host under a top-level domain that no other server
// has.
func splitIngressNginxServerAliases(value string) (aliases []string, invalid []string) {
	for _, alias := range strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' }) {
		if !ingressNginxServerAliasRegexp.MatchString(alias) ||
			strings.HasPrefix(alias, "*.") && strings.Count(alias, ".") < 2 {
			invalid = append(invalid, alias)
			continue
		}
		aliases = append(aliases, alias)
	}
	return aliases, invalid
}

// IngressNginxServerAliases are the names of server-alias the controller
// takes of the Ingress, lowercased: the hosts it claims besides those of its
// rules. The controller must treat them as hosts of the Ingress when it
// decides which resource holds a host, and drop with
// WithoutIngressNginxServerAliases those that another resource holds.
func IngressNginxServerAliases(ing *networking.Ingress) []string {
	value, exists := ing.Annotations[IngressNginxServerAliasAnnotation]
	if !exists {
		return nil
	}
	aliases, _ := splitIngressNginxServerAliases(strings.TrimSpace(value))
	for i := range aliases {
		aliases[i] = strings.ToLower(aliases[i])
	}
	return aliases
}

// WithoutIngressNginxServerAliases returns the Ingress without the names of
// its server-alias for which drop is true (it gets them lowercased), or the
// Ingress itself when it drops none. The Ingress it is given is not changed.
func WithoutIngressNginxServerAliases(ing *networking.Ingress, drop func(alias string) bool) *networking.Ingress {
	value, exists := ing.Annotations[IngressNginxServerAliasAnnotation]
	if !exists {
		return ing
	}
	aliases, _ := splitIngressNginxServerAliases(strings.TrimSpace(value))
	kept := make([]string, 0, len(aliases))
	for _, alias := range aliases {
		if !drop(strings.ToLower(alias)) {
			kept = append(kept, alias)
		}
	}
	if len(kept) == len(aliases) {
		return ing
	}
	copied := ing.DeepCopy()
	if len(kept) == 0 {
		delete(copied.Annotations, IngressNginxServerAliasAnnotation)
	} else {
		copied.Annotations[IngressNginxServerAliasAnnotation] = strings.Join(kept, ",")
	}
	return copied
}
