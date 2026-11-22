package configs

import (
	"crypto/sha1" // #nosec G505 -- the name of a location, as ingress-nginx makes it
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
)

// The external auth annotations of ingress-nginx.
const (
	IngressNginxAuthURLAnnotation                 = IngressNginxAnnotationPrefix + "auth-url"
	IngressNginxAuthMethodAnnotation              = IngressNginxAnnotationPrefix + "auth-method"
	IngressNginxAuthSigninAnnotation              = IngressNginxAnnotationPrefix + "auth-signin"
	IngressNginxAuthSigninRedirectParamAnnotation = IngressNginxAnnotationPrefix + "auth-signin-redirect-param"
	IngressNginxAuthResponseHeadersAnnotation     = IngressNginxAnnotationPrefix + "auth-response-headers"
	IngressNginxAuthRequestRedirectAnnotation     = IngressNginxAnnotationPrefix + "auth-request-redirect"
	IngressNginxAuthSnippetAnnotation             = IngressNginxAnnotationPrefix + "auth-snippet"
	IngressNginxAuthCacheKeyAnnotation            = IngressNginxAnnotationPrefix + "auth-cache-key"
	IngressNginxAuthCacheDurationAnnotation       = IngressNginxAnnotationPrefix + "auth-cache-duration"
	IngressNginxAuthAlwaysSetCookieAnnotation     = IngressNginxAnnotationPrefix + "auth-always-set-cookie"
	IngressNginxAuthProxySetHeadersAnnotation     = IngressNginxAnnotationPrefix + "auth-proxy-set-headers"
	IngressNginxAuthKeepaliveAnnotation           = IngressNginxAnnotationPrefix + "auth-keepalive"
)

const (
	ingressNginxDefaultSigninRedirectParam = "rd"
	ingressNginxDefaultAuthCacheDuration   = "200 202 401 5m"
	// ingressNginxAuthCacheDir holds the caches of the auth services, one directory per Ingress, which nginx makes.
	ingressNginxAuthCacheDir = "/var/cache/nginx"
)

var (
	// the validators of ingress-nginx
	// the host of an auth-url: a DNS name or IPv4 address, an IPv6 address in
	// brackets, each with an optional port; or $host or $server_name
	// without one ("$host:80" with a Host of "unix" would be a socket path)
	ingressNginxAuthHostRegexp         = regexp.MustCompile(`^(?:(?:[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?|\[[0-9a-fA-F:.]+\])(?::[0-9]{1,5})?|\$host|\$server_name)$`)
	ingressNginxURLWithVariablesRegexp = regexp.MustCompile(`^[\-\._~a-zA-Z0-9/:, ,:?&=$]*$`)
	ingressNginxURLRegexp              = regexp.MustCompile(`^[\-\._~a-zA-Z0-9/:,:?&=]*$`)
	ingressNginxMethodRegexp           = regexp.MustCompile(`^(GET|HEAD|POST|PUT|PATCH|DELETE|CONNECT|OPTIONS|TRACE)$`)
	ingressNginxHeadersRegexp          = regexp.MustCompile(`^[A-Za-z0-9-_, ]*$`)
	ingressNginxHeaderRegexp           = regexp.MustCompile(`^[a-zA-Z\d\-_]+$`)
	ingressNginxVariableRegexp         = regexp.MustCompile(`^[A-Za-z0-9\-\_\$\{\}]*$`)
	ingressNginxStatusCodeRegexp       = regexp.MustCompile(`^\d{3}$`)
	ingressNginxDurationRegexp         = regexp.MustCompile(`^\d+(ms|s|m|h|d|w|M|y)$`)
)

// ingressNginxExternalAuth is the external auth an Ingress asks for with the
// annotations of ingress-nginx, the same for each of its locations but for the
// names of the locations.
type ingressNginxExternalAuth struct {
	version1.IngressNginxExternalAuth
	signinURL           string
	signinRedirectParam string
	cacheKey            string
	// upstream is the upstream of the auth service with auth-keepalive.
	upstream *version1.IngressNginxAuthUpstream
}

// parseIngressNginxExternalAuth reads the external auth annotations. As
// ingress-nginx denies the locations whose auth-url, auth-method or
// auth-response-headers it cannot take, they answer 503; the same holds for
// the other annotations that make ingress-nginx refuse the Ingress, rather
// than serve the locations without their auth.
//
//gocyclo:ignore
func parseIngressNginxExternalAuth(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams, enableSnippets bool) *ingressNginxExternalAuth {
	authURL, exists := a.value(IngressNginxAuthURLAnnotation)
	if !exists {
		return nil
	}
	deny := func(format string, args ...interface{}) *ingressNginxExternalAuth {
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s", ingressNginxDeniedCode, fmt.Sprintf(format, args...))
		cfgParams.IngressNginxDenied = true
		return nil
	}

	authURL = strings.ReplaceAll(authURL, " ", "")
	if !ingressNginxURLWithVariablesRegexp.MatchString(authURL) {
		return deny("%s is invalid: %q", IngressNginxAuthURLAnnotation, authURL)
	}
	if err := checkIngressNginxVariables(authURL, false); err != nil {
		return deny("%s is invalid: %q: %v", IngressNginxAuthURLAnnotation, authURL, err)
	}
	parsed, err := url.Parse(authURL)
	switch {
	case err != nil:
		return deny("%s is no URL: %v", IngressNginxAuthURLAnnotation, err)
	case parsed.Scheme == "":
		return deny("%s has no scheme", IngressNginxAuthURLAnnotation)
	case parsed.Host == "" || strings.Contains(parsed.Host, ".."):
		return deny("%s has no valid host", IngressNginxAuthURLAnnotation)
	case parsed.Scheme != "http" && parsed.Scheme != "https":
		return deny("%s must be http or https: %q", IngressNginxAuthURLAnnotation, authURL)
	case !ingressNginxAuthHostRegexp.MatchString(parsed.Host):
		// nginx takes "unix:" in proxy_pass, also from a variable at request
		// time: an auth-url to a unix socket would reach the controller's own
		// APIs (the data plane module's), so the host is a name, an address
		// or nginx's own $host/$server_name, never a variable a client sets
		return deny("%s has no valid host: %q (a DNS name or an IP address with an optional port, or $host or $server_name)", IngressNginxAuthURLAnnotation, parsed.Host)
	}

	auth := &ingressNginxExternalAuth{}
	auth.URL = authURL
	auth.Host = parsed.Hostname()

	if method, exists := a.value(IngressNginxAuthMethodAnnotation); exists {
		if method = strings.ReplaceAll(method, " ", ""); !ingressNginxMethodRegexp.MatchString(method) {
			return deny("%s is no HTTP method: %q", IngressNginxAuthMethodAnnotation, method)
		}
		auth.Method = method
	}

	if headers, exists := a.value(IngressNginxAuthResponseHeadersAnnotation); exists {
		if !ingressNginxHeadersRegexp.MatchString(strings.ReplaceAll(headers, " ", "")) {
			return deny("%s is invalid: %q", IngressNginxAuthResponseHeadersAnnotation, headers)
		}
		for _, header := range strings.Split(headers, ",") {
			header = strings.TrimSpace(header)
			if header == "" {
				continue
			}
			if !ingressNginxHeaderRegexp.MatchString(header) {
				return deny("%s has an invalid header: %q", IngressNginxAuthResponseHeadersAnnotation, header)
			}
			auth.ResponseHeaders = append(auth.ResponseHeaders, version1.IngressNginxAuthHeader{
				Name:     header,
				Variable: "$upstream_http_" + strings.ReplaceAll(strings.ToLower(header), "-", "_"),
			})
		}
	}

	if redirect, exists := a.value(IngressNginxAuthRequestRedirectAnnotation); exists {
		if redirect = strings.ReplaceAll(redirect, " ", ""); !ingressNginxURLRegexp.MatchString(redirect) {
			return deny("%s is invalid: %q", IngressNginxAuthRequestRedirectAnnotation, redirect)
		}
		auth.RequestRedirect = redirect
	}

	if value, exists := a.value(IngressNginxAuthAlwaysSetCookieAnnotation); exists {
		var ok bool
		if auth.AlwaysSetCookie, ok = parseIngressNginxBool(value); !ok {
			return deny("%s must be a boolean, got %q", IngressNginxAuthAlwaysSetCookieAnnotation, value)
		}
	}

	if signin, exists := a.value(IngressNginxAuthSigninAnnotation); exists {
		if signin = strings.ReplaceAll(signin, " ", ""); !ingressNginxURLWithVariablesRegexp.MatchString(signin) {
			a.invalid(IngressNginxAuthSigninAnnotation, signin, "it must be a URL")
		} else if a.variables(IngressNginxAuthSigninAnnotation, signin, false) {
			auth.signinURL = signin
		}
	}
	auth.signinRedirectParam = ingressNginxDefaultSigninRedirectParam
	if param, exists := a.value(IngressNginxAuthSigninRedirectParamAnnotation); exists {
		if param = strings.ReplaceAll(param, " ", ""); ingressNginxURLRegexp.MatchString(param) {
			auth.signinRedirectParam = param
		} else {
			a.invalid(IngressNginxAuthSigninRedirectParamAnnotation, param, "it must be the name of a query parameter")
		}
	}

	if snippet, exists := a.value(IngressNginxAuthSnippetAnnotation); exists {
		if enableSnippets {
			auth.Snippet = snippet
		} else {
			a.warnings.AddWarningf(a.ing, "%s is ignored: snippets are not enabled", IngressNginxAuthSnippetAnnotation)
		}
	}

	if key, exists := a.value(IngressNginxAuthCacheKeyAnnotation); exists {
		if key = strings.ReplaceAll(key, " ", ""); !ingressNginxVariableRegexp.MatchString(key) {
			a.invalid(IngressNginxAuthCacheKeyAnnotation, key, "it must be letters, digits, -, _ and variables")
		} else if a.variables(IngressNginxAuthCacheKeyAnnotation, key, false) {
			auth.cacheKey = key
		}
	}
	durations := ingressNginxDefaultAuthCacheDuration
	if value, exists := a.value(IngressNginxAuthCacheDurationAnnotation); exists {
		durations = value
	}
	for _, duration := range strings.Split(durations, ",") {
		if duration = strings.TrimSpace(duration); duration == "" {
			continue
		}
		if !isIngressNginxCacheDuration(duration) {
			return deny("%s is invalid: %q", IngressNginxAuthCacheDurationAnnotation, duration)
		}
		auth.CacheDurations = append(auth.CacheDurations, duration)
	}
	if len(auth.CacheDurations) == 0 {
		auth.CacheDurations = []string{ingressNginxDefaultAuthCacheDuration}
	}
	if auth.cacheKey != "" {
		auth.CacheZone = ingressNginxAuthCacheZone(a.ing)
	}

	if !parseIngressNginxAuthProxySetHeaders(a, ingEx, cfgParams, auth) {
		return nil
	}
	parseIngressNginxAuthKeepalive(a, cfgParams, auth)
	return auth
}

// ReadSystemResolvers returns the name servers of the resolv.conf at path,
// IPv6 ones in brackets, as the resolver directive takes them. ingress-nginx
// resolves with them too.
func ReadSystemResolvers(path string) []string {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var resolvers []string
	for _, line := range strings.Split(string(content), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		ip := net.ParseIP(fields[1])
		switch {
		case ip == nil:
		case ip.To4() == nil:
			resolvers = append(resolvers, "["+ip.String()+"]")
		default:
			resolvers = append(resolvers, ip.String())
		}
	}
	return resolvers
}

// parseIngressNginxBool parses a boolean as strconv.ParseBool does.
func parseIngressNginxBool(value string) (bool, bool) {
	switch value {
	case "1", "t", "T", "TRUE", "true", "True":
		return true, true
	case "0", "f", "F", "FALSE", "false", "False":
		return false, true
	}
	return false, false
}

// isIngressNginxCacheDuration tells if the value is one of auth-cache-duration,
// [code ...] time ..., as ingress-nginx checks it.
func isIngressNginxCacheDuration(value string) bool {
	seenDuration := false
	for _, element := range strings.Split(value, " ") {
		switch {
		case element == "":
		case ingressNginxStatusCodeRegexp.MatchString(element):
			if seenDuration {
				return false
			}
		case ingressNginxDurationRegexp.MatchString(element):
			seenDuration = true
		}
	}
	return seenDuration
}

// ingressNginxAuthCacheZone returns the name of the cache of the answers of the auth service of the Ingress.
func ingressNginxAuthCacheZone(ing *networking.Ingress) string {
	return fmt.Sprintf("auth_cache_%s_%s", ing.Namespace, ing.Name)
}

// forLocation returns the external auth of the location of the path, with the
// names ingress-nginx gives its locations.
func (auth *ingressNginxExternalAuth) forLocation(host string, path string, pathType *networking.PathType) *version1.IngressNginxExternalAuth {
	locationAuth := auth.IngressNginxExternalAuth
	pathTypeName := "default"
	if pathType != nil {
		pathTypeName = string(*pathType)
	}
	locationAuth.Location = fmt.Sprintf("/_external-auth-%s-%s", base64.RawURLEncoding.EncodeToString([]byte(path)), pathTypeName)
	if auth.cacheKey != "" {
		locationAuth.CacheKey = host + locationAuth.Location + auth.cacheKey
	}
	if auth.signinURL != "" {
		hash := sha1.New() // #nosec G401 -- as ingress-nginx names the location
		hash.Write([]byte(path))
		hash.Write([]byte(auth.signinURL))
		locationAuth.SigninLocation = "@" + hex.EncodeToString(hash.Sum(nil))
		locationAuth.SigninURL = ingressNginxSigninURL(auth.signinURL, auth.signinRedirectParam)
	}
	return &locationAuth
}

// ingressNginxSigninURL appends the URL of the request to the sign-in URL in
// the redirect parameter, unless the sign-in URL has it already.
func ingressNginxSigninURL(signinURL string, param string) string {
	u, err := url.Parse(signinURL)
	if err != nil {
		return signinURL
	}
	q := u.Query()
	if len(q) == 0 {
		return fmt.Sprintf("%v?%v=$pass_access_scheme://$http_host$escaped_request_uri", signinURL, param)
	}
	if q.Get(param) != "" {
		return signinURL
	}
	return fmt.Sprintf("%v&%v=$pass_access_scheme://$http_host$escaped_request_uri", signinURL, param)
}

// applyIngressNginxExternalAuth puts the external auth of ingress-nginx on the
// location. add_header_inherit merge keeps the headers of the server, which a
// location's add_header Set-Cookie would drop otherwise.
func applyIngressNginxExternalAuth(loc *version1.Location, auth *ingressNginxExternalAuth, host string, path string, pathType *networking.PathType) {
	if auth == nil || loc.ACMEChallenge || loc.PoliciesErrorReturn != nil {
		return
	}
	loc.IngressNginxExternalAuth = auth.forLocation(host, path, pathType)
	if loc.AddHeaderInherit == "" {
		loc.AddHeaderInherit = addHeaderInheritMerge
	}
}

// applyIngressNginxLocation puts what the annotations of ingress-nginx ask
// for on the location of the path.
func applyIngressNginxLocation(loc *version1.Location, cfgParams *ConfigParams, extAuth *ingressNginxExternalAuth, host string, path string, pathType *networking.PathType) {
	applyIngressNginxRewrite(loc, cfgParams, path)
	loc.IngressNginxRedirect = cfgParams.IngressNginxRedirect
	applyIngressNginxRateLimit(loc, cfgParams)
	applyIngressNginxAuth(loc, cfgParams)
	applyIngressNginxExternalAuth(loc, extAuth, host, path, pathType)
}

// ingressNginxAuthCachesOf returns the cache the external auth of the Ingress needs, if any.
func ingressNginxAuthCachesOf(auth *ingressNginxExternalAuth) []version1.IngressNginxAuthCache {
	if auth == nil || auth.CacheZone == "" {
		return nil
	}
	return []version1.IngressNginxAuthCache{{Zone: auth.CacheZone, Path: ingressNginxAuthCacheDir + "/" + auth.CacheZone}}
}
