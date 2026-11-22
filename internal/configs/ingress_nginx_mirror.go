package configs

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version1"
)

// The mirror annotations of ingress-nginx.
const (
	IngressNginxMirrorTargetAnnotation      = IngressNginxAnnotationPrefix + "mirror-target"
	IngressNginxMirrorHostAnnotation        = IngressNginxAnnotationPrefix + "mirror-host"
	IngressNginxMirrorRequestBodyAnnotation = IngressNginxAnnotationPrefix + "mirror-request-body"
)

func init() {
	for _, name := range []string{IngressNginxMirrorTargetAnnotation, IngressNginxMirrorHostAnnotation, IngressNginxMirrorRequestBodyAnnotation} {
		ingressNginxRead[name] = true
	}
}

var (
	// a URL whose path may hold nginx variables, nothing that leaves the
	// string; a variable right after the host would let a client complete it
	// to "unix:/...sock:", a socket of the controller
	ingressNginxMirrorTargetRegexp = regexp.MustCompile(`^https?://[A-Za-z0-9.\-]+(:\d+)?(/[A-Za-z0-9\-._~/%?&=$]*)?$`)
	ingressNginxMirrorHostRegexp   = regexp.MustCompile(`^[A-Za-z0-9.\-]+(:\d+)?$`)
)

// parseIngressNginxMirror reads mirror-target, mirror-host and
// mirror-request-body: the requests of the Ingress are copied to the target,
// whose answer nobody waits for, as with ingress-nginx.
func parseIngressNginxMirror(a *ingressNginxAnnotations) *version1.IngressNginxMirror {
	target, ok := a.value(IngressNginxMirrorTargetAnnotation)
	if !ok {
		return nil
	}
	if !ingressNginxMirrorTargetRegexp.MatchString(target) {
		a.invalid(IngressNginxMirrorTargetAnnotation, target, "it must be an http or https URL")
		return nil
	}
	if !a.variables(IngressNginxMirrorTargetAnnotation, target, false) {
		return nil
	}
	m := &version1.IngressNginxMirror{
		Source:      "/_mirror-" + string(a.ing.UID),
		Target:      strings.ReplaceAll(target, "$request_uri", "$ingress_nginx_mirror_uri"),
		RequestBody: "on",
	}
	if a.ing.UID == "" {
		m.Source = "/_mirror-" + a.ing.Namespace + "-" + a.ing.Name
	}
	if v, ok := a.value(IngressNginxMirrorRequestBodyAnnotation); ok && v == "off" {
		m.RequestBody = "off"
	}
	if v, ok := a.value(IngressNginxMirrorHostAnnotation); ok {
		if ingressNginxMirrorHostRegexp.MatchString(v) {
			m.Host = v
		} else {
			a.invalid(IngressNginxMirrorHostAnnotation, v, "it must be a host name")
		}
	}
	if m.Host == "" {
		if u, err := url.Parse(strings.Split(target, "$")[0]); err == nil {
			m.Host = u.Hostname()
		}
	}
	return m
}
