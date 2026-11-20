package configs

import (
	"fmt"
	"net"
	"sort"
	"strings"
)

// The source-range annotations of ingress-nginx.
const (
	IngressNginxAllowlistSourceRangeAnnotation = IngressNginxAnnotationPrefix + "allowlist-source-range"
	IngressNginxWhitelistSourceRangeAnnotation = IngressNginxAnnotationPrefix + "whitelist-source-range"
	IngressNginxDenylistSourceRangeAnnotation  = IngressNginxAnnotationPrefix + "denylist-source-range"
)

// ingressNginxRead are the annotations of ingress-nginx wxingress reads
// (docs/user-guide/ingress-nginx-compatibility.md). TestIngressNginxReadIsComplete keeps it in step with the code.
var ingressNginxRead = map[string]bool{}

func init() {
	for _, name := range []string{
		"affinity", "affinity-canary-behavior", "affinity-mode",
		"allowlist-source-range", "whitelist-source-range", "denylist-source-range",
		"auth-always-set-cookie", "auth-cache-duration", "auth-cache-key", "auth-keepalive", "auth-method",
		"auth-proxy-set-headers", "auth-realm", "auth-request-redirect", "auth-response-headers", "auth-secret",
		"auth-secret-type", "auth-signin", "auth-signin-redirect-param", "auth-snippet", "auth-type", "auth-url",
		"backend-protocol",
		"canary", "canary-by-cookie", "canary-by-header", "canary-by-header-pattern", "canary-by-header-value",
		"canary-weight", "canary-weight-total",
		"client-body-buffer-size", "configuration-snippet", "server-snippet",
		"limit-allowlist", "limit-whitelist", "limit-burst-multiplier", "limit-connections", "limit-rate",
		"limit-rate-after", "limit-rpm", "limit-rps",
		"load-balance", "upstream-hash-by", "upstream-hash-by-subset", "upstream-hash-by-subset-size",
		"permanent-redirect", "permanent-redirect-code", "temporal-redirect", "temporal-redirect-code",
		"proxy-body-size", "proxy-buffering", "proxy-buffer-size", "proxy-buffers-number", "proxy-busy-buffers-size",
		"proxy-connect-timeout", "proxy-http-version", "proxy-max-temp-file-size", "proxy-next-upstream",
		"proxy-next-upstream-timeout", "proxy-next-upstream-tries", "proxy-read-timeout", "proxy-request-buffering",
		"proxy-send-timeout",
		"rewrite-target", "use-regex", "x-forwarded-prefix", "service-upstream",
		"session-cookie-change-on-failure", "session-cookie-conditional-samesite-none", "session-cookie-domain",
		"session-cookie-expires", "session-cookie-max-age", "session-cookie-name", "session-cookie-path",
		"session-cookie-samesite", "session-cookie-secure",
	} {
		ingressNginxRead[IngressNginxAnnotationPrefix+name] = true
	}
}

// ingressNginxFailClosed are annotations of ingress-nginx that restrict who
// gets an answer. wxingress does not do them yet; serving without them would
// serve what ingress-nginx refuses, so an Ingress with one answers 503 in all
// its locations instead, with a warning.
var ingressNginxFailClosed = map[string]string{
	"enable-modsecurity":              "ModSecurity (the WAF is wxwaf)",
	"enable-owasp-core-rules":         "ModSecurity (the WAF is wxwaf)",
	"modsecurity-snippet":             "ModSecurity (the WAF is wxwaf)",
	"modsecurity-transaction-id":      "ModSecurity (the WAF is wxwaf)",
	"global-rate-limit":               "the global rate limit (memcached)",
	"global-rate-limit-window":        "the global rate limit (memcached)",
	"global-rate-limit-key":           "the global rate limit (memcached)",
	"global-rate-limit-ignored-cidrs": "the global rate limit (memcached)",
}

// checkIngressNginxUnsupported warns about each ingress-nginx annotation
// wxingress does not read - ingress-nginx would do something with it, here it
// is ignored - and denies the locations of the Ingress (503) for those in
// ingressNginxFailClosed. A "false" of one that turns a restriction on (as
// enable-modsecurity: "false") restricts nothing and is only warned about.
func checkIngressNginxUnsupported(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	var ignored, denied []string
	for name, value := range a.ing.Annotations {
		if !strings.HasPrefix(name, IngressNginxAnnotationPrefix) || ingressNginxRead[name] {
			continue
		}
		short := strings.TrimPrefix(name, IngressNginxAnnotationPrefix)
		what, closed := ingressNginxFailClosed[short]
		value = strings.TrimSpace(value)
		if closed && !ingressNginxOff(short, value) {
			denied = append(denied, fmt.Sprintf("%s (%s)", name, what))
			continue
		}
		ignored = append(ignored, name)
	}
	sort.Strings(ignored)
	sort.Strings(denied)
	for _, name := range ignored {
		a.warnings.AddWarningf(a.ing, "%s is not supported by wxingress and is ignored", name)
	}
	if len(denied) > 0 {
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: wxingress does not support %s, and serving without it would serve what ingress-nginx restricts",
			ingressNginxDeniedCode, strings.Join(denied, ", "))
		cfgParams.IngressNginxDenied = true
	}
}

// ingressNginxOff tells that a fail-closed annotation restricts nothing with this value.
func ingressNginxOff(short, value string) bool {
	switch short {
	case "enable-modsecurity", "enable-owasp-core-rules":
		return value == "false"
	case "global-rate-limit":
		return value == "0" || value == ""
	case "modsecurity-transaction-id", "global-rate-limit-window",
		"global-rate-limit-key", "global-rate-limit-ignored-cidrs":
		// on their own they restrict nothing: auth-tls-secret, enable-modsecurity or global-rate-limit do
		return true
	}
	return false
}

// parseIngressNginxSourceRanges reads allowlist-source-range (whitelist-source-range
// as the older name) and denylist-source-range into the access rules of the
// locations, as ingress-nginx renders them: the denied ranges first, then the
// allowed ones and "deny all". An invalid range denies the locations (503), as
// ingress-nginx does.
func parseIngressNginxSourceRanges(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	var rules []string
	ranges := func(name string) ([]string, bool) {
		value, exists := a.value(name)
		if !exists {
			return nil, true
		}
		var out []string
		for _, r := range strings.Split(value, ",") {
			r = strings.TrimSpace(r)
			if r == "" {
				continue
			}
			if net.ParseIP(r) == nil {
				if _, _, err := net.ParseCIDR(r); err != nil {
					a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s has the invalid range %q", ingressNginxDeniedCode, name, r)
					cfgParams.IngressNginxDenied = true
					return nil, false
				}
			}
			out = append(out, r)
		}
		return out, true
	}

	deny, ok := ranges(IngressNginxDenylistSourceRangeAnnotation)
	if !ok {
		return
	}
	for _, r := range deny {
		rules = append(rules, "deny "+r)
	}
	allowName := IngressNginxAllowlistSourceRangeAnnotation
	if _, exists := a.ing.Annotations[allowName]; !exists {
		allowName = IngressNginxWhitelistSourceRangeAnnotation
	}
	allow, ok := ranges(allowName)
	if !ok {
		return
	}
	for _, r := range allow {
		rules = append(rules, "allow "+r)
	}
	if len(allow) > 0 {
		rules = append(rules, "deny all")
	}
	cfgParams.IngressNginxAccess = rules
}
