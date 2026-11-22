package configs

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/validation"
)

// IngressNginxAnnotationPrefix is the prefix of the annotations of ingress-nginx.
// The controller reads them with the semantics of ingress-nginx, so Ingresses
// written for it work unchanged.
const IngressNginxAnnotationPrefix = "nginx.ingress.kubernetes.io/"

// The proxy annotations of ingress-nginx, with the nginx.org annotation that does the same.
const (
	IngressNginxProxyConnectTimeoutAnnotation      = IngressNginxAnnotationPrefix + "proxy-connect-timeout"
	IngressNginxProxyReadTimeoutAnnotation         = IngressNginxAnnotationPrefix + "proxy-read-timeout"
	IngressNginxProxySendTimeoutAnnotation         = IngressNginxAnnotationPrefix + "proxy-send-timeout"
	IngressNginxProxyBodySizeAnnotation            = IngressNginxAnnotationPrefix + "proxy-body-size"
	IngressNginxClientBodyBufferSizeAnnotation     = IngressNginxAnnotationPrefix + "client-body-buffer-size"
	IngressNginxProxyBufferingAnnotation           = IngressNginxAnnotationPrefix + "proxy-buffering"
	IngressNginxProxyBufferSizeAnnotation          = IngressNginxAnnotationPrefix + "proxy-buffer-size"
	IngressNginxProxyBuffersNumberAnnotation       = IngressNginxAnnotationPrefix + "proxy-buffers-number"
	IngressNginxProxyBusyBuffersSizeAnnotation     = IngressNginxAnnotationPrefix + "proxy-busy-buffers-size"
	IngressNginxProxyMaxTempFileSizeAnnotation     = IngressNginxAnnotationPrefix + "proxy-max-temp-file-size"
	IngressNginxProxyRequestBufferingAnnotation    = IngressNginxAnnotationPrefix + "proxy-request-buffering"
	IngressNginxProxyHTTPVersionAnnotation         = IngressNginxAnnotationPrefix + "proxy-http-version"
	IngressNginxProxyNextUpstreamAnnotation        = IngressNginxAnnotationPrefix + "proxy-next-upstream"
	IngressNginxProxyNextUpstreamTimeoutAnnotation = IngressNginxAnnotationPrefix + "proxy-next-upstream-timeout"
	IngressNginxProxyNextUpstreamTriesAnnotation   = IngressNginxAnnotationPrefix + "proxy-next-upstream-tries"
)

// The rewrite annotations of ingress-nginx. A rewrite-target or use-regex
// makes every path of the server a case-insensitive regular expression.
const (
	IngressNginxRewriteTargetAnnotation    = IngressNginxAnnotationPrefix + "rewrite-target"
	IngressNginxUseRegexAnnotation         = IngressNginxAnnotationPrefix + "use-regex"
	IngressNginxXForwardedPrefixAnnotation = IngressNginxAnnotationPrefix + "x-forwarded-prefix"
)

// ingressNginxPathWithCaptureRegexp is the validator of rewrite-target and x-forwarded-prefix in ingress-nginx.
var ingressNginxPathWithCaptureRegexp = regexp.MustCompile(`^/?[\-\._~a-zA-Z0-9/:$]*$`)

// IngressNginxServiceUpstreamAnnotation makes the upstreams of the Ingress the
// ClusterIP of their Service instead of its endpoints, like nginx.org/use-cluster-ip.
const IngressNginxServiceUpstreamAnnotation = IngressNginxAnnotationPrefix + "service-upstream"

// The values ingress-nginx renders when its ConfigMap does not change them.
const (
	ingressNginxDefaultProxyBuffersNumber = 4
	ingressNginxDefaultProxyBufferSize    = "4k"
)

var (
	// ingressNginxOffsetRegexp and ingressNginxSizeRegexp are what ingress-nginx
	// renders of a size, an offset taking g as well.
	ingressNginxOffsetRegexp = regexp.MustCompile(`^\d+[kKmMgG]?$`)
	ingressNginxSizeRegexp   = regexp.MustCompile(`^\d+[kKmM]?$`)
	// ingressNginxNextUpstreamRegexp is the validator of proxy-next-upstream in ingress-nginx.
	ingressNginxNextUpstreamRegexp = regexp.MustCompile(`^((error|timeout|invalid_header|http_500|http_502|http_503|http_504|http_403|http_404|http_429|non_idempotent|off)\s?)+$`)
)

// ingressNginxAnnotations reads the ingress-nginx annotations of an Ingress
// and collects the warnings about those it ignores.
type ingressNginxAnnotations struct {
	ing      *networking.Ingress
	warnings Warnings
}

func newIngressNginxAnnotations(ing *networking.Ingress) *ingressNginxAnnotations {
	return &ingressNginxAnnotations{ing: ing, warnings: newWarnings()}
}

// value returns the ingress-nginx annotation, trimmed line by line as
// ingress-nginx does. It does not return it when one of the nginx.org
// annotations that set the same is there: that one wins, with a warning.
func (a *ingressNginxAnnotations) value(name string, nginxOrg ...string) (string, bool) {
	value, exists := a.ing.Annotations[name]
	if !exists {
		return "", false
	}
	for _, other := range nginxOrg {
		if _, set := a.ing.Annotations[other]; set {
			a.warnings.AddWarningf(a.ing, "%s is ignored: %s is set and takes precedence", name, other)
			return "", false
		}
	}
	lines := strings.Split(value, "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	value = strings.Join(lines, "\n")
	if value == "" {
		a.invalid(name, value, "it is empty")
		return "", false
	}
	return value, true
}

// invalid warns that the annotation is ignored, as ingress-nginx ignores an invalid annotation.
func (a *ingressNginxAnnotations) invalid(name string, value string, reason string) {
	a.warnings.AddWarningf(a.ing, "%s is ignored: %q is invalid: %s", name, value, reason)
}

// seconds returns an annotation that ingress-nginx takes as a whole number of seconds.
func (a *ingressNginxAnnotations) seconds(name string, nginxOrg ...string) (int, bool) {
	value, exists := a.value(name, nginxOrg...)
	if !exists {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < 0 {
		a.invalid(name, value, "it must be a number of seconds")
		return 0, false
	}
	return n, true
}

// intAtLeast returns an annotation that is a whole number of at least low.
func (a *ingressNginxAnnotations) intAtLeast(name string, low int, nginxOrg ...string) (int, bool) {
	value, exists := a.value(name, nginxOrg...)
	if !exists {
		return 0, false
	}
	n, err := strconv.Atoi(value)
	if err != nil || n < low {
		a.invalid(name, value, fmt.Sprintf("it must be a whole number of at least %d", low))
		return 0, false
	}
	return n, true
}

// matching returns an annotation that must match the regular expression.
func (a *ingressNginxAnnotations) matching(name string, re *regexp.Regexp, reason string, nginxOrg ...string) (string, bool) {
	value, exists := a.value(name, nginxOrg...)
	if !exists {
		return "", false
	}
	if !re.MatchString(value) {
		a.invalid(name, value, reason)
		return "", false
	}
	return value, true
}

// variables tells whether the value of the annotation has only variables nginx
// knows (checkIngressNginxVariables), and warns that it is ignored when not.
func (a *ingressNginxAnnotations) variables(name string, value string, bareBraces bool) bool {
	if err := checkIngressNginxVariables(value, bareBraces); err != nil {
		a.invalid(name, value, err.Error())
		return false
	}
	return true
}

// boolean returns an annotation that is a boolean, which ingress-nginx parses with strconv.ParseBool.
func (a *ingressNginxAnnotations) boolean(name string, nginxOrg ...string) (bool, bool) {
	value, exists := a.value(name, nginxOrg...)
	if !exists {
		return false, false
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		a.invalid(name, value, `it must be "true" or "false"`)
		return false, false
	}
	return b, true
}

// onOff returns an annotation that is "on" or "off".
func (a *ingressNginxAnnotations) onOff(name string, nginxOrg ...string) (bool, bool) {
	value, exists := a.value(name, nginxOrg...)
	if !exists {
		return false, false
	}
	switch value {
	case "on":
		return true, true
	case "off":
		return false, true
	}
	a.invalid(name, value, `it must be "on" or "off"`)
	return false, false
}

// parseIngressNginxAnnotations reads the ingress-nginx annotations of the
// Ingress into cfgParams, which already has those of nginx.org, and returns
// warnings about those it ignores.
func parseIngressNginxAnnotations(ingEx *IngressEx, cfgParams *ConfigParams, enableDirectiveAutoadjust bool, enableSnippets bool) Warnings {
	a := newIngressNginxAnnotations(ingEx.Ingress)
	parseIngressNginxProxyAnnotations(a, cfgParams, enableDirectiveAutoadjust)
	if useClusterIP, ok := a.boolean(IngressNginxServiceUpstreamAnnotation, UseClusterIPAnnotation); ok {
		cfgParams.UseClusterIP = useClusterIP
	}
	parseIngressNginxRewriteAnnotations(a, cfgParams)
	cfgParams.IngressNginxRedirect = parseIngressNginxRedirect(a)
	parseIngressNginxRateLimitAnnotations(a, ingEx, cfgParams)
	parseIngressNginxBasicAuthAnnotations(a, ingEx, cfgParams)
	parseIngressNginxAuthTLSAnnotations(a, ingEx, cfgParams)
	parseIngressNginxProxySSL(a, ingEx, cfgParams)
	cfgParams.IngressNginxExternalAuth = parseIngressNginxExternalAuth(a, ingEx, cfgParams, enableSnippets)
	cfgParams.IngressNginxAffinity = parseIngressNginxAffinity(a)
	parseIngressNginxLoadBalancing(a, cfgParams)
	parseIngressNginxSourceRanges(a, cfgParams)
	parseIngressNginxServerAnnotations(a, cfgParams)
	cfgParams.IngressNginxCORS = parseIngressNginxCORS(a)
	cfgParams.IngressNginxErrorCodes = parseIngressNginxCustomErrors(a)
	cfgParams.IngressNginxMirror = parseIngressNginxMirror(a)
	parseIngressNginxDirectives(a, cfgParams)
	parseIngressNginxConnection(a, cfgParams)
	parseIngressNginxCustomHeaders(a, ingEx, cfgParams)
	parseIngressNginxOtel(a, cfgParams)
	parseIngressNginxStreamSnippet(a, cfgParams, enableSnippets)
	parseIngressNginxFastCGI(a, ingEx, cfgParams)
	if server, location := parseIngressNginxSnippets(a, enableSnippets); server != nil || location != nil {
		if server != nil {
			cfgParams.ServerSnippets = server
		}
		if location != nil {
			cfgParams.LocationSnippets = location
		}
	}
	checkIngressNginxUnsupported(a, cfgParams)
	return a.warnings
}

// The snippet annotations of ingress-nginx.
const (
	IngressNginxServerSnippetAnnotation        = IngressNginxAnnotationPrefix + "server-snippet"
	IngressNginxConfigurationSnippetAnnotation = IngressNginxAnnotationPrefix + "configuration-snippet"
)

// parseIngressNginxSnippets reads server-snippet and configuration-snippet,
// what nginx.org/server-snippets and nginx.org/location-snippets are, which
// win over them. Like auth-snippet they need -enable-snippets and are
// ignored with a warning without it.
func parseIngressNginxSnippets(a *ingressNginxAnnotations, enableSnippets bool) (server []string, location []string) {
	read := func(name, nginxOrg string) []string {
		value, exists := a.value(name, nginxOrg)
		if !exists {
			return nil
		}
		if !enableSnippets {
			a.warnings.AddWarningf(a.ing, "%s is ignored: snippets are not enabled", name)
			return nil
		}
		return strings.Split(value, "\n")
	}
	return read(IngressNginxServerSnippetAnnotation, "nginx.org/server-snippets"),
		read(IngressNginxConfigurationSnippetAnnotation, "nginx.org/location-snippets")
}

// parseIngressNginxRewriteAnnotations reads rewrite-target, use-regex and
// x-forwarded-prefix. nginx.org/rewrites and nginx.org/rewrite-target win over
// rewrite-target, nginx.org/path-regex over use-regex.
func parseIngressNginxRewriteAnnotations(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	// ingress-nginx removes no spaces from rewrite-target before it validates it
	if target, ok := a.matching(IngressNginxRewriteTargetAnnotation, ingressNginxPathWithCaptureRegexp,
		"it must be a path of letters, digits, -._~/: and $1-style captures", RewriteTargetAnnotation, "nginx.org/rewrites"); ok &&
		a.variables(IngressNginxRewriteTargetAnnotation, target, false) {
		cfgParams.IngressNginxRewriteTarget = target
	}
	if useRegex, ok := a.boolean(IngressNginxUseRegexAnnotation, PathRegexAnnotation); ok {
		cfgParams.IngressNginxUseRegex = useRegex
	}
	if prefix, exists := a.value(IngressNginxXForwardedPrefixAnnotation); exists {
		if ingressNginxPathWithCaptureRegexp.MatchString(strings.ReplaceAll(prefix, " ", "")) {
			if a.variables(IngressNginxXForwardedPrefixAnnotation, prefix, true) {
				cfgParams.IngressNginxXForwardedPrefix = prefix
			}
		} else {
			a.invalid(IngressNginxXForwardedPrefixAnnotation, prefix, "it must be a path of letters, digits, -._~/: and $1-style captures")
		}
	}
}

// The redirect annotations of ingress-nginx.
const (
	IngressNginxPermanentRedirectAnnotation     = IngressNginxAnnotationPrefix + "permanent-redirect"
	IngressNginxPermanentRedirectCodeAnnotation = IngressNginxAnnotationPrefix + "permanent-redirect-code"
	IngressNginxTemporalRedirectAnnotation      = IngressNginxAnnotationPrefix + "temporal-redirect"
	IngressNginxTemporalRedirectCodeAnnotation  = IngressNginxAnnotationPrefix + "temporal-redirect-code"
)

// ingressNginxRedirectURLRegexp is a URL a redirect may go to: http or https,
// a host, and nothing that ends the directive or a quoted string. A $ only
// starts an nginx variable, as in https://example.org$request_uri.
var ingressNginxRedirectURLRegexp = regexp.MustCompile(`^https?://[^\s;{}"'\\` + "`" + `$]+(\$[a-z_][a-z0-9_]*[^\s;{}"'\\` + "`" + `$]*)*$`)

// parseIngressNginxRedirect reads permanent-redirect and temporal-redirect, as
// ingress-nginx does (internal/ingress/annotations/redirect): the temporal one
// wins, with code 302 unless temporal-redirect-code says otherwise; the
// permanent one has 301 unless permanent-redirect-code says otherwise. A code
// outside 300-308 is the default, with a warning.
func parseIngressNginxRedirect(a *ingressNginxAnnotations) *version2.Return {
	redirect := func(urlAnnotation, codeAnnotation string, defaultCode int) *version2.Return {
		target, exists := a.value(urlAnnotation)
		if !exists {
			return nil
		}
		target = strings.TrimSpace(target)
		if !ingressNginxRedirectURLRegexp.MatchString(target) {
			a.invalid(urlAnnotation, target, "it must be an http or https URL")
			return nil
		}
		if !a.variables(urlAnnotation, target, true) {
			return nil
		}
		code := defaultCode
		if value, exists := a.value(codeAnnotation); exists {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			if err != nil || n < 300 || n > 308 {
				a.invalid(codeAnnotation, value, fmt.Sprintf("it must be a redirect code from 300 to 308; %d is used", defaultCode))
			} else {
				code = n
			}
		}
		return &version2.Return{Code: code, Text: target}
	}
	if r := redirect(IngressNginxTemporalRedirectAnnotation, IngressNginxTemporalRedirectCodeAnnotation, 302); r != nil {
		return r
	}
	return redirect(IngressNginxPermanentRedirectAnnotation, IngressNginxPermanentRedirectCodeAnnotation, 301)
}

// applyIngressNginxRewrite applies the rewrite of ingress-nginx to the location
// of the path: rewrite "(?i)path" target break, unless the target is the path,
// and X-Forwarded-Prefix with it.
func applyIngressNginxRewrite(loc *version1.Location, cfgParams *ConfigParams, path string) {
	target := cfgParams.IngressNginxRewriteTarget
	if target != "" && target != path {
		loc.RewriteTarget = target
		loc.CaseInsensitiveRewrite = true
		loc.UseRegex = true
		if cfgParams.IngressNginxXForwardedPrefix != "" {
			loc.ProxySetHeaders = append(slices.Clone(loc.ProxySetHeaders), version2.Header{Name: "X-Forwarded-Prefix", Value: cfgParams.IngressNginxXForwardedPrefix})
		}
	}
	if cfgParams.IngressNginxUseRegex {
		loc.UseRegex = true
	}
}

// enforceIngressNginxRegex renders every path of the server as ~* "^path" when
// one of its locations has use-regex or a rewrite-target of ingress-nginx, and
// orders them as ingress-nginx does: the longest path first and, of paths of
// the same length, the greater one first, since nginx tries regular
// expressions in order. Internal and ACME challenge locations keep their place
// and their match, as do paths with nginx.org/path-regex.
func enforceIngressNginxRegex(server *version1.Server, ingressAnnotations map[string]string) {
	if !slices.ContainsFunc(server.Locations, func(loc version1.Location) bool { return loc.UseRegex }) {
		return
	}

	var indexes []int
	var paths []version1.Location
	for i := range server.Locations {
		loc := &server.Locations[i]
		if loc.Internal || loc.ACMEChallenge {
			continue
		}
		if _, hasPathRegex := pathRegexOf(loc, ingressAnnotations); !hasPathRegex {
			loc.CaseInsensitiveRegex = true
		}
		indexes = append(indexes, i)
		paths = append(paths, *loc)
	}

	rawPath := func(loc version1.Location) string { return strings.TrimPrefix(loc.Path, "= ") }
	sort.SliceStable(paths, func(i, j int) bool { return rawPath(paths[i]) > rawPath(paths[j]) })
	sort.SliceStable(paths, func(i, j int) bool { return len(rawPath(paths[i])) > len(rawPath(paths[j])) })
	for n, i := range indexes {
		server.Locations[i] = paths[n]
	}
}

// pathRegexOf returns the nginx.org/path-regex of the location: that of its
// minion or, for other locations, that of the Ingress.
func pathRegexOf(loc *version1.Location, ingressAnnotations map[string]string) (string, bool) {
	if loc.MinionIngress != nil && loc.MinionIngress.Annotations["nginx.org/mergeable-ingress-type"] == "minion" {
		regexType, hasRegex := loc.MinionIngress.Annotations[PathRegexAnnotation]
		return regexType, hasRegex
	}
	regexType, hasRegex := ingressAnnotations[PathRegexAnnotation]
	return regexType, hasRegex
}

// UsesClusterIP tells if the upstreams of the Ingress are the ClusterIP of
// their Service: with nginx.org/use-cluster-ip "true" or, when that is not
// set, with service-upstream of ingress-nginx "true". As ingress-nginx does,
// the controller takes the endpoints of a Service that has no ClusterIP.
func UsesClusterIP(ing *networking.Ingress) bool {
	if value, exists := ing.Annotations[UseClusterIPAnnotation]; exists {
		return value == "true"
	}
	useClusterIP, err := strconv.ParseBool(strings.TrimSpace(ing.Annotations[IngressNginxServiceUpstreamAnnotation]))
	return err == nil && useClusterIP
}

// parseIngressNginxProxyAnnotations reads the timeouts, sizes and buffering of
// ingress-nginx. Its timeouts are whole seconds and rendered with an s, the
// timeout of proxy-next-upstream-timeout as the bare number, as ingress-nginx does.
//
//gocyclo:ignore
func parseIngressNginxProxyAnnotations(a *ingressNginxAnnotations, cfgParams *ConfigParams, enableDirectiveAutoadjust bool) {
	if n, ok := a.seconds(IngressNginxProxyConnectTimeoutAnnotation, "nginx.org/proxy-connect-timeout"); ok {
		cfgParams.ProxyConnectTimeout = fmt.Sprintf("%ds", n)
	}
	if n, ok := a.seconds(IngressNginxProxyReadTimeoutAnnotation, "nginx.org/proxy-read-timeout"); ok {
		cfgParams.ProxyReadTimeout = fmt.Sprintf("%ds", n)
	}
	if n, ok := a.seconds(IngressNginxProxySendTimeoutAnnotation, "nginx.org/proxy-send-timeout"); ok {
		cfgParams.ProxySendTimeout = fmt.Sprintf("%ds", n)
	}

	if size, ok := a.matching(IngressNginxProxyBodySizeAnnotation, ingressNginxOffsetRegexp, "it must be a size like 8m or 1g", "nginx.org/client-max-body-size"); ok {
		cfgParams.ClientMaxBodySize = size
	}
	if size, ok := a.matching(IngressNginxClientBodyBufferSizeAnnotation, ingressNginxSizeRegexp, "it must be a size like 16k or 1m", "nginx.org/client-body-buffer-size"); ok {
		cfgParams.ClientBodyBufferSize = size
	}

	if buffering, ok := a.onOff(IngressNginxProxyBufferingAnnotation, "nginx.org/proxy-buffering"); ok {
		cfgParams.ProxyBuffering = buffering
	}
	if buffering, ok := a.onOff(IngressNginxProxyRequestBufferingAnnotation); ok {
		cfgParams.ProxyRequestBuffering = "off"
		if buffering {
			cfgParams.ProxyRequestBuffering = "on"
		}
	}

	// ingress-nginx renders proxy_buffer_size S and proxy_buffers N S, so either annotation sets both
	bufferSize, hasBufferSize := a.matching(IngressNginxProxyBufferSizeAnnotation, ingressNginxSizeRegexp, "it must be a size like 8k", "nginx.org/proxy-buffer-size", "nginx.org/proxy-buffers")
	buffersNumber, hasBuffersNumber := a.intAtLeast(IngressNginxProxyBuffersNumberAnnotation, 1, "nginx.org/proxy-buffers", "nginx.org/proxy-buffer-size")
	if hasBufferSize || hasBuffersNumber {
		configuredNumber, configuredSize := splitProxyBuffers(cfgParams.ProxyBuffers)
		if !hasBufferSize {
			bufferSize = ingressNginxDefaultProxyBufferSize
			if cfgParams.ProxyBufferSize != "" {
				bufferSize = cfgParams.ProxyBufferSize
			} else if configuredSize != "" {
				bufferSize = configuredSize
			}
		}
		if !hasBuffersNumber {
			buffersNumber = ingressNginxDefaultProxyBuffersNumber
			if configuredNumber > 0 {
				buffersNumber = configuredNumber
			}
		}
		cfgParams.ProxyBufferSize = bufferSize
		cfgParams.ProxyBuffers = fmt.Sprintf("%d %s", buffersNumber, bufferSize)
	}
	busyBuffersSize, hasBusyBuffersSize := a.matching(IngressNginxProxyBusyBuffersSizeAnnotation, ingressNginxSizeRegexp, "it must be a size like 16k", "nginx.org/proxy-busy-buffers-size")
	if hasBusyBuffersSize {
		cfgParams.ProxyBusyBuffersSize = busyBuffersSize
	}
	if size, ok := a.matching(IngressNginxProxyMaxTempFileSizeAnnotation, ingressNginxOffsetRegexp, "it must be a size like 1024m", "nginx.org/proxy-max-temp-file-size"); ok {
		cfgParams.ProxyMaxTempFileSize = size
	}
	if (hasBufferSize || hasBuffersNumber || hasBusyBuffersSize) && enableDirectiveAutoadjust {
		cfgParams.ProxyBuffers, cfgParams.ProxyBufferSize, cfgParams.ProxyBusyBuffersSize, _ = validation.BalanceProxyValues(cfgParams.ProxyBuffers, cfgParams.ProxyBufferSize, cfgParams.ProxyBusyBuffersSize, true)
	}

	if version, exists := a.value(IngressNginxProxyHTTPVersionAnnotation, ProxyHTTPVersionAnnotation); exists {
		if version == "1.0" || version == "1.1" {
			cfgParams.ProxyHTTPVersion = version
		} else {
			a.invalid(IngressNginxProxyHTTPVersionAnnotation, version, `it must be "1.0" or "1.1"`)
		}
	}

	if nextUpstream, ok := a.matching(IngressNginxProxyNextUpstreamAnnotation, ingressNginxNextUpstreamRegexp,
		"it must be a list of error, timeout, invalid_header, http_500, http_502, http_503, http_504, http_403, http_404, http_429, non_idempotent or off", ProxyNextUpstreamAnnotation); ok {
		cfgParams.ProxyNextUpstream = ingressNginxNextUpstream(nextUpstream)
	}
	if n, ok := a.seconds(IngressNginxProxyNextUpstreamTimeoutAnnotation, ProxyNextUpstreamTimeoutAnnotation); ok {
		cfgParams.ProxyNextUpstreamTimeout = strconv.Itoa(n)
	}
	if n, ok := a.intAtLeast(IngressNginxProxyNextUpstreamTriesAnnotation, 0, ProxyNextUpstreamTriesAnnotation); ok {
		tries := uint64(n)
		cfgParams.ProxyNextUpstreamTries = &tries
	}
}

// ingressNginxNextUpstream renders proxy-next-upstream as ingress-nginx does,
// with non_idempotent moved to the end.
func ingressNginxNextUpstream(value string) string {
	var conditions []string
	nonIdempotent := false
	for _, condition := range strings.Fields(value) {
		if condition == "non_idempotent" {
			nonIdempotent = true
			continue
		}
		conditions = append(conditions, condition)
	}
	if nonIdempotent {
		conditions = append(conditions, "non_idempotent")
	}
	return strings.Join(conditions, " ")
}

// splitProxyBuffers splits the value of proxy_buffers into its number and size.
func splitProxyBuffers(value string) (int, string) {
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, ""
	}
	n, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, ""
	}
	return n, fields[1]
}

// addIngressNginxWarnings adds the warnings to all, keeping those all already has for the same object.
func addIngressNginxWarnings(all Warnings, warnings Warnings) {
	for obj, msgs := range warnings {
		all[obj] = append(all[obj], msgs...)
	}
}
