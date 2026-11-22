package configs

import (
	"regexp"
	"sort"
	"strings"
)

// Annotations of ingress-nginx that are one nginx directive each in the locations of the Ingress.
const (
	IngressNginxEnableRewriteLogAnnotation      = IngressNginxAnnotationPrefix + "enable-rewrite-log"
	IngressNginxProxyCookieDomainAnnotation     = IngressNginxAnnotationPrefix + "proxy-cookie-domain"
	IngressNginxProxyCookiePathAnnotation       = IngressNginxAnnotationPrefix + "proxy-cookie-path"
	IngressNginxRelativeRedirectsAnnotation     = IngressNginxAnnotationPrefix + "relative-redirects"
	IngressNginxUsePortInRedirectsAnnotation    = IngressNginxAnnotationPrefix + "use-port-in-redirects"
	IngressNginxSatisfyAnnotation               = IngressNginxAnnotationPrefix + "satisfy"
	IngressNginxDisableProxyInterceptAnnotation = IngressNginxAnnotationPrefix + "disable-proxy-intercept-errors"
)

func init() {
	for _, name := range []string{
		IngressNginxEnableRewriteLogAnnotation, IngressNginxProxyCookieDomainAnnotation,
		IngressNginxProxyCookiePathAnnotation, IngressNginxRelativeRedirectsAnnotation, IngressNginxUsePortInRedirectsAnnotation,
		IngressNginxSatisfyAnnotation, IngressNginxDisableProxyInterceptAnnotation,
	} {
		ingressNginxRead[name] = true
	}
}

var (
	ingressNginxCookieRuleRegexp = regexp.MustCompile(`^[A-Za-z0-9.\-_/~*$^()|]+ [A-Za-z0-9.\-_/~$]+$|^off$`)
)

// checkIngressNginxCookieRule checks what nginx -t would refuse of
// proxy_cookie_domain or proxy_cookie_path from to: a regular expression
// (~ or ~*) PCRE does not compile, which nginx compiles as it is, without
// variables, or a variable it does not know.
func checkIngressNginxCookieRule(rule string) error {
	if rule == "off" {
		return nil
	}
	from, to, _ := strings.Cut(rule, " ")
	pattern, isRegex := strings.CutPrefix(from, "~*")
	if !isRegex {
		pattern, isRegex = strings.CutPrefix(from, "~")
	}
	if isRegex {
		if err := CheckPCRERegex(pattern); err != nil {
			return err
		}
	} else if err := checkIngressNginxVariables(from, false); err != nil {
		return err
	}
	return checkIngressNginxVariables(to, false)
}

// parseIngressNginxDirectives reads the annotations that are one directive
// each, as ingress-nginx renders them in the locations.
func parseIngressNginxDirectives(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	var d []string
	if v, ok := a.boolean(IngressNginxEnableRewriteLogAnnotation); ok && v {
		d = append(d, "rewrite_log on")
	}
	for name, directive := range map[string]string{IngressNginxProxyCookieDomainAnnotation: "proxy_cookie_domain", IngressNginxProxyCookiePathAnnotation: "proxy_cookie_path"} {
		if v, ok := a.value(name); ok {
			v = strings.Join(strings.Fields(v), " ")
			if ingressNginxCookieRuleRegexp.MatchString(v) {
				if err := checkIngressNginxCookieRule(v); err != nil {
					a.invalid(name, v, err.Error())
				} else {
					d = append(d, directive+" "+v)
				}
			} else {
				a.invalid(name, v, `it must be "<from> <to>" or off`)
			}
		}
	}
	if v, ok := a.boolean(IngressNginxRelativeRedirectsAnnotation); ok && v {
		d = append(d, "absolute_redirect off")
	}
	if v, ok := a.boolean(IngressNginxUsePortInRedirectsAnnotation); ok {
		if v {
			d = append(d, "port_in_redirect on")
		} else {
			d = append(d, "port_in_redirect off")
		}
	}
	if v, ok := a.value(IngressNginxSatisfyAnnotation); ok {
		switch v {
		case "any", "all":
			d = append(d, "satisfy "+v)
		default:
			a.invalid(IngressNginxSatisfyAnnotation, v, "it must be any or all")
		}
	}
	if v, ok := a.boolean(IngressNginxDisableProxyInterceptAnnotation); ok && v {
		cfgParams.IngressNginxErrorCodes = nil
	}
	sort.Strings(d)
	cfgParams.IngressNginxDirectives = d
}
