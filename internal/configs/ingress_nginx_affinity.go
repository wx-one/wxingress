package configs

import (
	"fmt"
	"regexp"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/nginx"
)

// The session affinity and load balancing annotations of ingress-nginx.
const (
	IngressNginxAffinityAnnotation                      = IngressNginxAnnotationPrefix + "affinity"
	IngressNginxAffinityModeAnnotation                  = IngressNginxAnnotationPrefix + "affinity-mode"
	IngressNginxAffinityCanaryBehaviorAnnotation        = IngressNginxAnnotationPrefix + "affinity-canary-behavior"
	IngressNginxSessionCookieNameAnnotation             = IngressNginxAnnotationPrefix + "session-cookie-name"
	IngressNginxSessionCookiePathAnnotation             = IngressNginxAnnotationPrefix + "session-cookie-path"
	IngressNginxSessionCookieDomainAnnotation           = IngressNginxAnnotationPrefix + "session-cookie-domain"
	IngressNginxSessionCookieExpiresAnnotation          = IngressNginxAnnotationPrefix + "session-cookie-expires"
	IngressNginxSessionCookieMaxAgeAnnotation           = IngressNginxAnnotationPrefix + "session-cookie-max-age"
	IngressNginxSessionCookieSecureAnnotation           = IngressNginxAnnotationPrefix + "session-cookie-secure"
	IngressNginxSessionCookieSameSiteAnnotation         = IngressNginxAnnotationPrefix + "session-cookie-samesite"
	IngressNginxSessionCookieConditionalSameSiteNoneAnn = IngressNginxAnnotationPrefix + "session-cookie-conditional-samesite-none"
	IngressNginxSessionCookieChangeOnFailureAnnotation  = IngressNginxAnnotationPrefix + "session-cookie-change-on-failure"
	IngressNginxUpstreamHashByAnnotation                = IngressNginxAnnotationPrefix + "upstream-hash-by"
	IngressNginxUpstreamHashBySubsetAnnotation          = IngressNginxAnnotationPrefix + "upstream-hash-by-subset"
	IngressNginxUpstreamHashBySubsetSizeAnnotation      = IngressNginxAnnotationPrefix + "upstream-hash-by-subset-size"
	IngressNginxLoadBalanceAnnotation                   = IngressNginxAnnotationPrefix + "load-balance"
)

const (
	ingressNginxDefaultAffinityCookieName = "INGRESSCOOKIE"
	// ingressNginxDefaultSubsetSize is upstream-hash-by-subset-size when it is not set.
	ingressNginxDefaultSubsetSize = 3
	// ingressNginxSameSiteNoneVariable is none, or empty for the browsers that
	// reject SameSite=None, as session-cookie-conditional-samesite-none asks.
	ingressNginxSameSiteNoneVariable = "$ingress_nginx_samesite_none"
)

var (
	ingressNginxAffinityAgeRegexp = regexp.MustCompile(`^(0|[1-9]\d*)$`)
	// letters, digits, - and _ and variables, $name or ${name}: no bare
	// brace, as the key is unquoted in hash ... consistent
	ingressNginxHashByRegexp = regexp.MustCompile(`^[A-Za-z0-9\-_${}]*$`)
)

// ingressNginxAffinity is the cookie affinity an Ingress asks for with the annotations of ingress-nginx.
type ingressNginxAffinity struct {
	cookieName string
	// path is session-cookie-path; without it the cookie has the path of the Ingress.
	path     string
	domain   string
	expires  string
	maxAge   string
	secure   bool
	sameSite string
	// conditionalSameSiteNone leaves SameSite=None out for the browsers that reject it.
	conditionalSameSiteNone bool
	// legacyCanary is affinity-canary-behavior legacy: the canary is not sticky with the affinity of its primary.
	legacyCanary bool
	// persistent is affinity-mode persistent: a session stays on its endpoint when the backend scales.
	persistent bool
	// changeOnFailure is session-cookie-change-on-failure: a retry after a failure goes to another endpoint.
	changeOnFailure bool
}

// parseIngressNginxAffinity reads affinity cookie and the session-cookie-*
// annotations. nginx.org/sticky-cookie-services wins for the services it names.
//
//gocyclo:ignore
func parseIngressNginxAffinity(a *ingressNginxAnnotations) *ingressNginxAffinity {
	affinityType, exists := a.value(IngressNginxAffinityAnnotation)
	if !exists {
		return nil
	}
	if affinityType = strings.TrimSpace(affinityType); affinityType != "cookie" {
		a.invalid(IngressNginxAffinityAnnotation, affinityType, `the only affinity is "cookie"`)
		return nil
	}

	affinity := &ingressNginxAffinity{cookieName: ingressNginxDefaultAffinityCookieName}
	if mode, exists := a.value(IngressNginxAffinityModeAnnotation); exists {
		switch strings.TrimSpace(mode) {
		case "balanced":
		case "persistent":
			affinity.persistent = true
		default:
			a.invalid(IngressNginxAffinityModeAnnotation, mode, `it must be "balanced" or "persistent"`)
		}
	}
	if behavior, exists := a.value(IngressNginxAffinityCanaryBehaviorAnnotation); exists {
		switch strings.TrimSpace(behavior) {
		case "sticky":
		case "legacy":
			affinity.legacyCanary = true
		default:
			a.invalid(IngressNginxAffinityCanaryBehaviorAnnotation, behavior, `it must be "sticky" or "legacy"`)
		}
	}
	if name, exists := a.value(IngressNginxSessionCookieNameAnnotation); exists {
		if name = strings.ReplaceAll(name, " ", ""); name != "" && ingressNginxBasicCharsRegexp.MatchString(name) && canaryNameRe.MatchString(name) && len(name) <= canaryMaxName {
			affinity.cookieName = name
		} else {
			a.invalid(IngressNginxSessionCookieNameAnnotation, name, "it must be the name of a cookie")
		}
	}
	if path, exists := a.value(IngressNginxSessionCookiePathAnnotation); exists {
		if path = strings.ReplaceAll(path, " ", ""); ingressNginxURLRegexp.MatchString(path) && !strings.Contains(path, "$") {
			affinity.path = path
		} else {
			a.invalid(IngressNginxSessionCookiePathAnnotation, path, "it must be a path")
		}
	}
	if domain, exists := a.value(IngressNginxSessionCookieDomainAnnotation); exists {
		if domain = strings.ReplaceAll(domain, " ", ""); ingressNginxBasicCharsRegexp.MatchString(domain) {
			affinity.domain = domain
		} else {
			a.invalid(IngressNginxSessionCookieDomainAnnotation, domain, "it must be a domain")
		}
	}
	for _, age := range []struct {
		name  string
		value *string
	}{{IngressNginxSessionCookieExpiresAnnotation, &affinity.expires}, {IngressNginxSessionCookieMaxAgeAnnotation, &affinity.maxAge}} {
		if value, exists := a.value(age.name); exists {
			if ingressNginxAffinityAgeRegexp.MatchString(value) {
				*age.value = value
			} else {
				a.invalid(age.name, value, "it must be a number of seconds")
			}
		}
	}
	if secure, ok := a.boolean(IngressNginxSessionCookieSecureAnnotation); ok {
		affinity.secure = secure
	}
	if sameSite, exists := a.value(IngressNginxSessionCookieSameSiteAnnotation); exists {
		// ingress-nginx sets SameSite only for these values, and drops any other
		switch sameSite {
		case "Strict", "Lax", "None":
			affinity.sameSite = strings.ToLower(sameSite)
		default:
			a.invalid(IngressNginxSessionCookieSameSiteAnnotation, sameSite, `ingress-nginx sets SameSite only for "Strict", "Lax" and "None"`)
		}
	}
	if conditional, ok := a.boolean(IngressNginxSessionCookieConditionalSameSiteNoneAnn); ok {
		affinity.conditionalSameSiteNone = conditional
	}
	if change, ok := a.boolean(IngressNginxSessionCookieChangeOnFailureAnnotation); ok {
		affinity.changeOnFailure = change
	}
	return affinity
}

// stickyCookie returns the parameters of sticky cookie for an upstream of the
// paths, on a server that redirects to HTTPS or not. The cookie has
// HttpOnly, as in ingress-nginx, Secure where every request comes over HTTPS
// when session-cookie-secure does not ask for it, and the path of the
// Ingress: of several paths that share the upstream, their common prefix.
func (affinity *ingressNginxAffinity) stickyCookie(paths []string, onlyHTTPS bool) string {
	params := []string{affinity.cookieName}
	// nginx renders expires=T as Expires and Max-Age, and browsers take Max-Age
	if affinity.maxAge != "" {
		params = append(params, "expires="+affinity.maxAge+"s")
	} else if affinity.expires != "" {
		params = append(params, "expires="+affinity.expires+"s")
	}
	if affinity.domain != "" {
		params = append(params, "domain="+affinity.domain)
	}
	params = append(params, "httponly")
	if affinity.secure || onlyHTTPS {
		params = append(params, "secure")
	}
	switch {
	case affinity.sameSite == "none" && affinity.conditionalSameSiteNone:
		params = append(params, "samesite="+ingressNginxSameSiteNoneVariable)
	case affinity.sameSite != "":
		params = append(params, "samesite="+affinity.sameSite)
	}
	path := affinity.path
	if path == "" {
		path = commonPathPrefix(paths)
	}
	params = append(params, "path="+path)
	return strings.Join(params, " ")
}

// balancer returns the rule of the wxingress module for an upstream of the
// paths with the affinity: sticky_persistent or sticky_balanced, as
// get_implementation of ingress-nginx names them, with sticky.lua's cookie.
// The cookie is Secure where session-cookie-secure asks for it, or where the
// request came over TLS, which the module sees per request.
func (affinity *ingressNginxAffinity) balancer(backend string, paths []string) nginx.Balancer {
	balancer := nginx.Balancer{
		Algorithm:                     "sticky_balanced",
		Backend:                       backend,
		CookieName:                    affinity.cookieName,
		CookiePath:                    affinity.path,
		CookieDomain:                  affinity.domain,
		CookieExpires:                 affinity.expires,
		CookieMaxAge:                  affinity.maxAge,
		CookieSecure:                  affinity.secure,
		CookieConditionalSameSiteNone: affinity.conditionalSameSiteNone,
		ChangeOnFailure:               affinity.changeOnFailure,
	}
	if affinity.persistent {
		balancer.Algorithm = "sticky_persistent"
	}
	if balancer.CookiePath == "" {
		balancer.CookiePath = commonPathPrefix(paths)
	}
	// resty.cookie writes SameSite only as Strict, Lax or None
	switch affinity.sameSite {
	case "strict":
		balancer.CookieSameSite = "Strict"
	case "lax":
		balancer.CookieSameSite = "Lax"
	case "none":
		balancer.CookieSameSite = "None"
	}
	return balancer
}

// commonPathPrefix returns the path, or of several the longest prefix of whole segments they share.
func commonPathPrefix(paths []string) string {
	if len(paths) == 0 {
		return "/"
	}
	prefix := paths[0]
	for _, path := range paths[1:] {
		if path == prefix {
			continue
		}
		segments, other := strings.Split(prefix, "/"), strings.Split(path, "/")
		n := 0
		for n < len(segments) && n < len(other) && segments[n] == other[n] {
			n++
		}
		// a cookie of path /a goes with /a and /a/..., not with /ab
		if prefix = strings.Join(segments[:n], "/"); prefix == "" {
			return "/"
		}
	}
	return prefix
}

// ingressNginxBalancing is the load balancing an Ingress asks for with
// upstream-hash-by and load-balance, as the wxingress module does it.
type ingressNginxBalancing struct {
	// algorithm is chash, chashsubset, round_robin or ewma; "" when only the affinity balances.
	algorithm  string
	hashBy     string
	subsetSize int
	// nginxLBMethod is the method of the upstreams before the annotations of
	// ingress-nginx changed it, which they keep with -dynamic-upstreams: the
	// module balances them, and the configuration stays the same whatever they say.
	nginxLBMethod string
}

// balancer returns the rule of the wxingress module for an upstream, or false without one.
func (b *ingressNginxBalancing) balancer(backend string) (nginx.Balancer, bool) {
	if b == nil || b.algorithm == "" {
		return nginx.Balancer{}, false
	}
	balancer := nginx.Balancer{Algorithm: b.algorithm, HashBy: b.hashBy, Backend: backend}
	if b.algorithm == "chashsubset" {
		balancer.SubsetSize = b.subsetSize
	}
	return balancer, true
}

// parseIngressNginxLoadBalancing reads upstream-hash-by and load-balance into
// the method of the upstreams, after the affinity, which wins as in
// ingress-nginx. nginx.org/lb-method wins over all of them. The balancing is
// kept in cfgParams.IngressNginxBalancing too, for the wxingress module.
//
//gocyclo:ignore
func parseIngressNginxLoadBalancing(a *ingressNginxAnnotations, cfgParams *ConfigParams) {
	const nginxOrgLBMethod = "nginx.org/lb-method"
	balancing := &ingressNginxBalancing{nginxLBMethod: cfgParams.LBMethod}
	if cfgParams.IngressNginxAffinity != nil {
		if _, set := a.ing.Annotations[nginxOrgLBMethod]; !set {
			// new sessions go to a random endpoint, as both modes of ingress-nginx pick them
			cfgParams.LBMethod = "random"
		}
		cfgParams.IngressNginxBalancing = balancing
		return
	}

	if hashBy, exists := a.value(IngressNginxUpstreamHashByAnnotation, nginxOrgLBMethod); exists {
		if hashBy = strings.ReplaceAll(hashBy, " ", ""); !ingressNginxHashByRegexp.MatchString(hashBy) {
			a.invalid(IngressNginxUpstreamHashByAnnotation, hashBy, "it must be letters, digits, - and _ and variables")
		} else if a.variables(IngressNginxUpstreamHashByAnnotation, hashBy, false) {
			cfgParams.LBMethod = fmt.Sprintf("hash %s consistent", hashBy)
			balancing.algorithm, balancing.hashBy = "chash", hashBy
			if subset, ok := a.boolean(IngressNginxUpstreamHashBySubsetAnnotation); ok && subset {
				balancing.algorithm, balancing.subsetSize = "chashsubset", ingressNginxDefaultSubsetSize
				if size, ok := a.intAtLeast(IngressNginxUpstreamHashBySubsetSizeAnnotation, 1); ok {
					balancing.subsetSize = size
				}
			}
			cfgParams.IngressNginxBalancing = balancing
			return
		}
	}

	if method, exists := a.value(IngressNginxLoadBalanceAnnotation, nginxOrgLBMethod); exists {
		switch method = strings.TrimSpace(method); method {
		case "round_robin":
			cfgParams.LBMethod = ""
			balancing.algorithm = method
			cfgParams.IngressNginxBalancing = balancing
		case "ewma":
			// the latency of whole responses, as ewma weighs it; nginx has no decaying average of it
			cfgParams.LBMethod = "least_time last_byte"
			balancing.algorithm = method
			cfgParams.IngressNginxBalancing = balancing
		case "chash", "chashsubset", "sticky_balanced", "sticky_persistent":
			a.warnings.AddWarningf(a.ing, "%s %s is ignored: it needs upstream-hash-by or affinity", IngressNginxLoadBalanceAnnotation, method)
		default:
			a.invalid(IngressNginxLoadBalanceAnnotation, method, `it must be "round_robin" or "ewma"`)
		}
	}
}

// ingressNginxStickyUpstreams collects the paths of each upstream of an
// Ingress with affinity, to give each its sticky cookie once all are known,
// and with -dynamic-upstreams the balancing of each upstream and the name
// ingress-nginx has for its backend, to give each its balancer rule.
type ingressNginxStickyUpstreams struct {
	affinity  map[string]*ingressNginxAffinity
	paths     map[string][]string
	onlyHTTPS map[string]bool
	balancing map[string]*ingressNginxBalancing
	backend   map[string]string
}

func newIngressNginxStickyUpstreams() *ingressNginxStickyUpstreams {
	return &ingressNginxStickyUpstreams{
		affinity:  make(map[string]*ingressNginxAffinity),
		paths:     make(map[string][]string),
		onlyHTTPS: make(map[string]bool),
		balancing: make(map[string]*ingressNginxBalancing),
		backend:   make(map[string]string),
	}
}

// add records that the upstream with the affinity serves the path on a server
// that redirects to HTTPS or not.
func (s *ingressNginxStickyUpstreams) add(upstream string, affinity *ingressNginxAffinity, path string, onlyHTTPS bool) {
	if affinity == nil {
		return
	}
	if _, seen := s.affinity[upstream]; !seen {
		s.affinity[upstream] = affinity
		s.onlyHTTPS[upstream] = onlyHTTPS
	}
	s.onlyHTTPS[upstream] = s.onlyHTTPS[upstream] && onlyHTTPS
	s.paths[upstream] = append(s.paths[upstream], path)
}

// balance records the balancing of the upstream and the backend of
// ingress-nginx it stands for, the Service and port in the namespace.
func (s *ingressNginxStickyUpstreams) balance(upstream string, balancing *ingressNginxBalancing, namespace string, backend *networking.IngressBackend) {
	if _, seen := s.backend[upstream]; !seen {
		s.backend[upstream] = ingressNginxBackendName(namespace, backend)
	}
	if balancing != nil {
		if _, seen := s.balancing[upstream]; !seen {
			s.balancing[upstream] = balancing
		}
	}
}

// IngressNginxBackendName is upstreamName of ingress-nginx, the name of its backend of a Service's port: <namespace>-<service>-<port number or name>.
func IngressNginxBackendName(namespace string, backend *networking.IngressBackend) string {
	return ingressNginxBackendName(namespace, backend)
}

// ingressNginxBackendName is upstreamName of ingress-nginx: <namespace>-<service>-<port number or name>.
func ingressNginxBackendName(namespace string, backend *networking.IngressBackend) string {
	if backend != nil && backend.Service != nil {
		if backend.Service.Port.Number > 0 {
			return fmt.Sprintf("%s-%s-%d", namespace, backend.Service.Name, backend.Service.Port.Number)
		}
		if backend.Service.Port.Name != "" {
			return fmt.Sprintf("%s-%s-%s", namespace, backend.Service.Name, backend.Service.Port.Name)
		}
	}
	return namespace + "-INVALID"
}

// apply gives the upstreams their sticky cookie, but those with one of nginx.org/sticky-cookie-services.
func (s *ingressNginxStickyUpstreams) apply(upstreams map[string]version1.Upstream) {
	for name, affinity := range s.affinity {
		upstream, exists := upstreams[name]
		if !exists || upstream.StickyCookie != "" {
			continue
		}
		upstream.StickyCookie = affinity.stickyCookie(s.paths[name], s.onlyHTTPS[name])
		upstreams[name] = upstream
	}
}

// balancers returns the balancer rules of the upstreams for the wxingress
// module, as get_implementation of ingress-nginx chooses the algorithm: the
// affinity, else upstream-hash-by, else load-balance. An upstream with a
// cookie of nginx.org/sticky-cookie-services keeps nginx's sticky.
func (s *ingressNginxStickyUpstreams) balancers(upstreams map[string]version1.Upstream) map[string]nginx.Balancer {
	var rules map[string]nginx.Balancer
	set := func(name string, rule nginx.Balancer) {
		if rules == nil {
			rules = make(map[string]nginx.Balancer)
		}
		rules[name] = rule
	}
	for name, upstream := range upstreams {
		if upstream.StickyCookie != "" {
			continue
		}
		if affinity, exists := s.affinity[name]; exists {
			set(name, affinity.balancer(s.backend[name], s.paths[name]))
			continue
		}
		if rule, ok := s.balancing[name].balancer(s.backend[name]); ok {
			set(name, rule)
		}
	}
	return rules
}

// stickyCookieName returns the name of the cookie of the parameters of sticky cookie.
func stickyCookieName(stickyCookie string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(stickyCookie), " ")
	return name
}

// canaryBalancing returns the upstream-hash-by and load-balance of a canary
// Ingress, which ingress-nginx builds the canary's backend from.
func canaryBalancing(canary *networking.Ingress) *ingressNginxBalancing {
	cfgParams := &ConfigParams{}
	parseIngressNginxLoadBalancing(newIngressNginxAnnotations(canary), cfgParams)
	return cfgParams.IngressNginxBalancing
}

// canaryAffinity returns the affinity of the canary upstream of a primary with
// the affinity: the same, as ingress-nginx gives it, or with
// affinity-canary-behavior legacy that of the canary Ingress, if it has one.
func canaryAffinity(primary *ingressNginxAffinity, canary *networking.Ingress) *ingressNginxAffinity {
	if primary == nil || !primary.legacyCanary {
		return primary
	}
	return parseIngressNginxAffinity(newIngressNginxAnnotations(canary))
}
