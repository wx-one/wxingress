package configs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/nginx"
	networking "k8s.io/api/networking/v1"
)

// The canary annotations of ingress-nginx. With -dynamic-upstreams an Ingress
// with CanaryAnnotation "true" makes no servers of its own: its backends become
// the canaries of the Ingress that has the same host, path and path type.
const (
	CanaryAnnotation                = "nginx.ingress.kubernetes.io/canary"
	CanaryWeightAnnotation          = "nginx.ingress.kubernetes.io/canary-weight"
	CanaryWeightTotalAnnotation     = "nginx.ingress.kubernetes.io/canary-weight-total"
	CanaryByHeaderAnnotation        = "nginx.ingress.kubernetes.io/canary-by-header"
	CanaryByHeaderValueAnnotation   = "nginx.ingress.kubernetes.io/canary-by-header-value"
	CanaryByHeaderPatternAnnotation = "nginx.ingress.kubernetes.io/canary-by-header-pattern"
	CanaryByCookieAnnotation        = "nginx.ingress.kubernetes.io/canary-by-cookie"
)

// The room the wxingress module has for the parts of a canary rule.
const (
	canaryMaxUpstreamName = 127
	canaryMaxName         = 63
	canaryMaxValue        = 255
	canaryDefaultTotal    = 100
)

var canaryNameRe = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// IsCanaryIngress tells if the Ingress is a canary Ingress of ingress-nginx.
func IsCanaryIngress(ing *networking.Ingress) bool {
	enabled, err := strconv.ParseBool(ing.Annotations[CanaryAnnotation])
	return err == nil && enabled
}

// ParseCanaryAnnotations parses the canary annotations of a canary Ingress into
// a rule without its canary upstream, which depends on the path it is used for.
func ParseCanaryAnnotations(ing *networking.Ingress) (nginx.Canary, error) {
	canary := nginx.Canary{WeightTotal: canaryDefaultTotal}
	annotations := ing.Annotations

	if value, exists := annotations[CanaryWeightAnnotation]; exists {
		weight, err := strconv.Atoi(value)
		if err != nil || weight < 0 {
			return canary, fmt.Errorf("%s must be a non-negative integer, got %q", CanaryWeightAnnotation, value)
		}
		canary.Weight = weight
	}

	if value, exists := annotations[CanaryWeightTotalAnnotation]; exists {
		total, err := strconv.Atoi(value)
		if err != nil || total <= 0 {
			return canary, fmt.Errorf("%s must be a positive integer, got %q", CanaryWeightTotalAnnotation, value)
		}
		canary.WeightTotal = total
	}

	canary.Header = annotations[CanaryByHeaderAnnotation]
	if canary.Header != "" && (len(canary.Header) > canaryMaxName || !canaryNameRe.MatchString(canary.Header)) {
		return canary, fmt.Errorf("%s must be the name of a header, got %q", CanaryByHeaderAnnotation, canary.Header)
	}

	canary.HeaderValue = annotations[CanaryByHeaderValueAnnotation]
	if len(canary.HeaderValue) > canaryMaxValue || !isCanaryText(canary.HeaderValue, false) {
		return canary, fmt.Errorf("%s must have at most %d characters and no quote, backslash or control character", CanaryByHeaderValueAnnotation, canaryMaxValue)
	}

	canary.HeaderPattern = annotations[CanaryByHeaderPatternAnnotation]
	if len(canary.HeaderPattern) > canaryMaxValue || !isCanaryText(canary.HeaderPattern, true) {
		return canary, fmt.Errorf("%s must have at most %d characters and no quote or control character", CanaryByHeaderPatternAnnotation, canaryMaxValue)
	}
	if _, err := regexp.CompilePOSIX(canary.HeaderPattern); err != nil {
		return canary, fmt.Errorf("%s must be an extended regular expression: %w", CanaryByHeaderPatternAnnotation, err)
	}

	canary.Cookie = annotations[CanaryByCookieAnnotation]
	if canary.Cookie != "" && (len(canary.Cookie) > canaryMaxName || !canaryNameRe.MatchString(canary.Cookie)) {
		return canary, fmt.Errorf("%s must be the name of a cookie, got %q", CanaryByCookieAnnotation, canary.Cookie)
	}

	return canary, nil
}

// isCanaryText tells if the wxingress module takes the text as it is.
func isCanaryText(s string, backslashes bool) bool {
	for _, c := range []byte(s) {
		if c == '"' || c < 0x20 || c == 0x7f || (c == '\\' && !backslashes) {
			return false
		}
	}
	return true
}

// canaryVariableReplacer escapes what a variable's name may not have, and the
// capitals nginx would lower: an upstream of a Gateway route starts with GW-.
var canaryVariableReplacer = strings.NewReplacer(canaryVariableEscapes()...)

func canaryVariableEscapes() []string {
	escapes := []string{"_", "__", "-", "_d", ".", "_p"}
	for c := 'A'; c <= 'Z'; c++ {
		escapes = append(escapes, string(c), "_u"+string(c-'A'+'a'))
	}
	return escapes
}

// canaryVariable returns the variable of the wxingress module that names the
// primary upstream or its canary, request by request.
// For example, default-cafe.example.com-tea-80 has $meta_canary_default_dcafe_pexample_pcom_dtea_d80
// and vs_ns_GW-1_be-2 has $meta_canary_vs__ns___ug_uw_d1__be_d2.
func canaryVariable(upstream string) string {
	return "$meta_canary_" + canaryVariableReplacer.Replace(upstream)
}

// canaryForPath returns the canary Ingress of a path of the Ingress and the
// backend it has for it: of the canaries with the same host, path and path
// type, the first one, since ingress-nginx uses only the first alternative backend.
// A canary of an upstream (CanaryOf) is the canary of every path whose backend
// is that upstream, whatever its host and path, as ingress-nginx's is.
func canaryForPath(ingEx *IngressEx, host string, path *networking.HTTPIngressPath) (*IngressEx, *networking.IngressBackend) {
	upstream := ""
	for _, canaryEx := range ingEx.Canaries {
		if canaryEx.CanaryOf != nil {
			if upstream == "" {
				upstream = ingressNginxBackendName(ingEx.Ingress.Namespace, &path.Backend)
			}
			if backend, ok := canaryEx.CanaryOf[upstream]; ok {
				return canaryEx, &backend
			}
			continue
		}
		for _, rule := range canaryEx.Ingress.Spec.Rules {
			if rule.Host != host || rule.HTTP == nil {
				continue
			}
			for i := range rule.HTTP.Paths {
				p := &rule.HTTP.Paths[i]
				if p.Backend.Service == nil || pathOrDefault(p.Path) != pathOrDefault(path.Path) || !samePathType(p.PathType, path.PathType) {
					continue
				}
				return canaryEx, &p.Backend
			}
		}
	}
	return nil, nil
}

func samePathType(a *networking.PathType, b *networking.PathType) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// canaryUpstreamName returns the name of the upstream of the canary for the
// location of the primary upstream, or an error if the wxingress module has no
// room for one of the names.
func canaryUpstreamName(primary string, canaryEx *IngressEx, host string, backend *networking.IngressBackend) (string, error) {
	name := getNameForUpstream(canaryEx.Ingress, host, backend)
	if len(primary) > canaryMaxUpstreamName || len(name) > canaryMaxUpstreamName {
		return "", fmt.Errorf("the canary %s/%s is ignored for upstream %s: upstream names of a canary have at most %d characters",
			canaryEx.Ingress.Namespace, canaryEx.Ingress.Name, primary, canaryMaxUpstreamName)
	}
	return name, nil
}
