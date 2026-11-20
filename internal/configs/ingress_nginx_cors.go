package configs

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version1"
)

// The CORS annotations of ingress-nginx.
const (
	IngressNginxEnableCORSAnnotation           = IngressNginxAnnotationPrefix + "enable-cors"
	IngressNginxCORSAllowOriginAnnotation      = IngressNginxAnnotationPrefix + "cors-allow-origin"
	IngressNginxCORSAllowMethodsAnnotation     = IngressNginxAnnotationPrefix + "cors-allow-methods"
	IngressNginxCORSAllowHeadersAnnotation     = IngressNginxAnnotationPrefix + "cors-allow-headers"
	IngressNginxCORSAllowCredentialsAnnotation = IngressNginxAnnotationPrefix + "cors-allow-credentials"
	IngressNginxCORSExposeHeadersAnnotation    = IngressNginxAnnotationPrefix + "cors-expose-headers"
	IngressNginxCORSMaxAgeAnnotation           = IngressNginxAnnotationPrefix + "cors-max-age"
)

func init() {
	for _, name := range []string{
		IngressNginxEnableCORSAnnotation, IngressNginxCORSAllowOriginAnnotation, IngressNginxCORSAllowMethodsAnnotation,
		IngressNginxCORSAllowHeadersAnnotation, IngressNginxCORSAllowCredentialsAnnotation,
		IngressNginxCORSExposeHeadersAnnotation, IngressNginxCORSMaxAgeAnnotation,
	} {
		ingressNginxRead[name] = true
	}
}

// ingress-nginx's defaults and validators of CORS
const (
	ingressNginxCORSMethods = "GET, PUT, POST, DELETE, PATCH, OPTIONS"
	ingressNginxCORSHeaders = "DNT,Keep-Alive,User-Agent,X-Requested-With,If-Modified-Since,Cache-Control,Content-Type,Range,Authorization"
	ingressNginxCORSMaxAge  = 1728000
)

var (
	ingressNginxCORSOriginRegexp  = regexp.MustCompile(`^([a-z]+://(\*\.)?[A-Za-z0-9\-.]*(:\d+)?|\*|null)?$`)
	ingressNginxCORSMethodsRegexp = regexp.MustCompile(`^([A-Za-z]+,?\s?)+$`)
	ingressNginxCORSHeadersRegexp = regexp.MustCompile(`^[A-Za-z0-9-_, ]*$`)
	ingressNginxCORSExposeRegexp  = regexp.MustCompile(`^(([A-Za-z0-9\-\_]+|\*),?\s?)+$`)
)

// parseIngressNginxCORS reads enable-cors and the cors-* annotations as
// ingress-nginx does: an invalid value is its default (with a warning), an
// invalid origin is left out.
func parseIngressNginxCORS(a *ingressNginxAnnotations) *version1.IngressNginxCORS {
	enabled, ok := a.boolean(IngressNginxEnableCORSAnnotation)
	if !ok || !enabled {
		return nil
	}
	cors := &version1.IngressNginxCORS{
		Methods:     ingressNginxCORSMethods,
		Headers:     ingressNginxCORSHeaders,
		Credentials: true,
		MaxAge:      ingressNginxCORSMaxAge,
	}
	if origins, ok := a.value(IngressNginxCORSAllowOriginAnnotation); ok {
		var regexes []string
		for _, origin := range strings.Split(origins, ",") {
			origin = strings.TrimSpace(origin)
			if origin == "" {
				continue
			}
			if origin == "*" {
				regexes = nil
				cors.AnyOrigin = true
				break
			}
			if !ingressNginxCORSOriginRegexp.MatchString(origin) {
				a.invalid(IngressNginxCORSAllowOriginAnnotation, origin, "it is not an origin")
				continue
			}
			// ingress-nginx's buildOriginRegex: quoted, the first * a label
			re := strings.Replace(regexp.QuoteMeta(origin), `\*`, `[A-Za-z0-9\-]+`, 1)
			regexes = append(regexes, "("+re+")")
		}
		if !cors.AnyOrigin {
			cors.OriginRegex = "(" + strings.Join(regexes, "|") + ")$"
			if len(regexes) == 0 {
				cors.OriginRegex = ""
			}
		}
	} else {
		cors.AnyOrigin = true
	}
	if v, ok := a.value(IngressNginxCORSAllowHeadersAnnotation); ok {
		if ingressNginxCORSHeadersRegexp.MatchString(v) {
			cors.Headers = v
		} else {
			a.invalid(IngressNginxCORSAllowHeadersAnnotation, v, "it must be header names separated by commas")
		}
	}
	if v, ok := a.value(IngressNginxCORSAllowMethodsAnnotation); ok {
		if ingressNginxCORSMethodsRegexp.MatchString(v) {
			cors.Methods = v
		} else {
			a.invalid(IngressNginxCORSAllowMethodsAnnotation, v, "it must be methods separated by commas")
		}
	}
	if v, ok := a.boolean(IngressNginxCORSAllowCredentialsAnnotation); ok {
		cors.Credentials = v
	}
	if v, ok := a.value(IngressNginxCORSExposeHeadersAnnotation); ok {
		if ingressNginxCORSExposeRegexp.MatchString(v) {
			cors.ExposeHeaders = v
		} else {
			a.invalid(IngressNginxCORSExposeHeadersAnnotation, v, "it must be header names separated by commas")
		}
	}
	if v, ok := a.value(IngressNginxCORSMaxAgeAnnotation); ok {
		if n, err := strconv.Atoi(v); err == nil {
			cors.MaxAge = n
		} else {
			a.invalid(IngressNginxCORSMaxAgeAnnotation, v, fmt.Sprintf("it must be a number, %d is taken", ingressNginxCORSMaxAge))
		}
	}
	return cors
}
