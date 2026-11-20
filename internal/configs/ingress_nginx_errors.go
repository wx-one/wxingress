package configs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version1"
	networking "k8s.io/api/networking/v1"
)

// ingress-nginx's custom error pages and the backend of an Ingress's errors.
const (
	IngressNginxCustomHTTPErrorsAnnotation = IngressNginxAnnotationPrefix + "custom-http-errors"
	IngressNginxDefaultBackendAnnotation   = IngressNginxAnnotationPrefix + "default-backend"
)

func init() {
	ingressNginxRead[IngressNginxCustomHTTPErrorsAnnotation] = true
	ingressNginxRead[IngressNginxDefaultBackendAnnotation] = true
}

var ingressNginxServiceNameRegexp = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// IngressNginxDefaultBackendService is the Service of the Ingress's default-backend annotation, if it is a valid name.
func IngressNginxDefaultBackendService(ing *networking.Ingress) (string, bool) {
	name := strings.TrimSpace(ing.Annotations[IngressNginxDefaultBackendAnnotation])
	if name == "" || !ingressNginxServiceNameRegexp.MatchString(name) {
		return "", false
	}
	return name, true
}

// parseIngressNginxCustomErrors reads custom-http-errors: the codes, each 300-599 (what error_page takes).
func parseIngressNginxCustomErrors(a *ingressNginxAnnotations) []int {
	value, ok := a.value(IngressNginxCustomHTTPErrorsAnnotation)
	if !ok {
		return nil
	}
	var codes []int
	for _, c := range strings.Split(value, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(c))
		if err != nil || n < 300 || n > 599 {
			a.invalid(IngressNginxCustomHTTPErrorsAnnotation, value, "it must be HTTP error codes separated by commas")
			return nil
		}
		codes = append(codes, n)
	}
	return codes
}

// ingressNginxErrorLocations are the named locations a location's errors go
// to, as ingress-nginx's @custom_<upstream>_<code>: to the backend of the
// errors with X-Code, X-Format, X-Original-URI, X-Namespace, X-Ingress-Name,
// X-Service-Name (the Service of the location), X-Request-ID, and Host.
func ingressNginxErrorLocations(backend string, codes []int) []version1.IngressNginxErrorLocation {
	out := make([]version1.IngressNginxErrorLocation, 0, len(codes))
	for _, c := range codes {
		out = append(out, version1.IngressNginxErrorLocation{Name: "@custom_" + backend + "_" + strconv.Itoa(c), Code: c, Upstream: backend})
	}
	return out
}

// applyIngressNginxErrors sends the errors of custom-http-errors from the
// locations of the server to the default-backend, and what nginx answers 502
// or 503 itself (no endpoint, none reachable) as well, keeping the request.
// withFallback also sends nginx's own 502 and 503 there (the default-backend
// annotation); the controller's default backend gets the codes only.
func applyIngressNginxErrors(server *version1.Server, locations []version1.Location, backend string, codes []int, withFallback bool) {
	pages := ingressNginxErrorLocations(backend, codes)
	fallback := version1.IngressNginxErrorLocation{Name: "@inx_default_backend_" + backend, Upstream: backend}
	for i := range locations {
		if locations[i].ACMEChallenge || locations[i].PoliciesErrorReturn != nil {
			continue
		}
		locations[i].IngressNginxErrorPages = pages
		if withFallback {
			locations[i].IngressNginxFallback = fallback.Name
		}
	}
	add := func(l version1.IngressNginxErrorLocation) {
		// the controller's default backend is that of every Ingress of the server: each of its locations once
		for _, have := range server.IngressNginxErrorLocations {
			if have.Name == l.Name {
				return
			}
		}
		server.IngressNginxErrorLocations = append(server.IngressNginxErrorLocations, l)
	}
	for _, p := range pages {
		add(p)
	}
	if withFallback {
		add(fallback)
	}
}
