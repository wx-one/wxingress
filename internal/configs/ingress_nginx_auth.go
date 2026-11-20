package configs

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
)

// The basic auth annotations of ingress-nginx.
const (
	IngressNginxAuthTypeAnnotation       = IngressNginxAnnotationPrefix + "auth-type"
	IngressNginxAuthSecretAnnotation     = IngressNginxAnnotationPrefix + "auth-secret"
	IngressNginxAuthSecretTypeAnnotation = IngressNginxAnnotationPrefix + "auth-secret-type"
	IngressNginxAuthRealmAnnotation      = IngressNginxAnnotationPrefix + "auth-realm"
)

// ingressNginxDeniedCode is what ingress-nginx answers in a location it denies,
// for example one whose auth-secret does not exist.
const ingressNginxDeniedCode = 503

var (
	// ingressNginxBasicCharsRegexp is the validator of auth-secret in ingress-nginx.
	ingressNginxBasicCharsRegexp = regexp.MustCompile(`^[/\-\._~a-zA-Z0-9:]*$`)
	// ingressNginxCharsWithSpaceRegexp is the validator of auth-realm in ingress-nginx.
	ingressNginxCharsWithSpaceRegexp = regexp.MustCompile(`^[/\-\._~a-zA-Z0-9: ]*$`)
)

// ingressNginxBasicAuth is the basic auth an Ingress asks for with the annotations of ingress-nginx.
type ingressNginxBasicAuth struct {
	// secretKey is namespace/name of the Secret, role says how its data makes the htpasswd file.
	secretKey string
	role      secrets.SecretRole
	realm     string
	// denied tells why the locations of the Ingress answer 503 instead, if they do.
	denied string
}

// parseIngressNginxBasicAuth reads auth-type, auth-secret, auth-secret-type and
// auth-realm. It returns false when the Ingress has no basic auth of
// ingress-nginx: without auth-type, or with nginx.org/basic-auth-secret, which
// wins. As ingress-nginx denies a location whose auth it cannot set up, an
// auth that is not valid denies them: they answer 503 rather than serve
// without the auth they ask for.
func parseIngressNginxBasicAuth(a *ingressNginxAnnotations) (ingressNginxBasicAuth, bool) {
	authType, exists := a.value(IngressNginxAuthTypeAnnotation, BasicAuthSecretAnnotation)
	if !exists {
		return ingressNginxBasicAuth{}, false
	}
	deny := func(format string, args ...interface{}) (ingressNginxBasicAuth, bool) {
		reason := fmt.Sprintf(format, args...)
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s", ingressNginxDeniedCode, reason)
		return ingressNginxBasicAuth{denied: reason}, true
	}

	switch authType = strings.ReplaceAll(authType, " ", ""); authType {
	case "basic":
	case "digest":
		return deny("%s digest needs the digest auth module, which open-source nginx does not have", IngressNginxAuthTypeAnnotation)
	default:
		return deny("%s must be basic or digest, got %q", IngressNginxAuthTypeAnnotation, authType)
	}

	auth := ingressNginxBasicAuth{role: secrets.RoleAuthFile}
	if secretType, exists := a.value(IngressNginxAuthSecretTypeAnnotation); exists {
		switch strings.ReplaceAll(secretType, " ", "") {
		case "auth-file":
		case "auth-map":
			auth.role = secrets.RoleAuthMap
		default:
			return deny("%s must be auth-file or auth-map, got %q", IngressNginxAuthSecretTypeAnnotation, secretType)
		}
	}

	secretName, exists := a.value(IngressNginxAuthSecretAnnotation)
	if !exists {
		return deny("%s is not set", IngressNginxAuthSecretAnnotation)
	}
	secretName = strings.ReplaceAll(secretName, " ", "")
	if !ingressNginxBasicCharsRegexp.MatchString(secretName) {
		return deny("%s is invalid: %q", IngressNginxAuthSecretAnnotation, secretName)
	}
	namespace, name := a.ing.Namespace, secretName
	switch parts := strings.Split(secretName, "/"); len(parts) {
	case 1:
	case 2:
		if parts[0] != "" {
			namespace = parts[0]
		}
		name = parts[1]
	default:
		return deny("%s is invalid: %q", IngressNginxAuthSecretAnnotation, secretName)
	}
	if namespace != a.ing.Namespace {
		return deny("%s %s is in another namespace, which ingress-nginx does not allow", IngressNginxAuthSecretAnnotation, secretName)
	}
	if name == "" {
		return deny("%s is invalid: %q", IngressNginxAuthSecretAnnotation, secretName)
	}
	auth.secretKey = namespace + "/" + name

	if realm, exists := a.ing.Annotations[IngressNginxAuthRealmAnnotation]; exists {
		realm = strings.TrimSpace(realm)
		if !ingressNginxCharsWithSpaceRegexp.MatchString(realm) {
			return deny("%s is invalid: %q", IngressNginxAuthRealmAnnotation, realm)
		}
		auth.realm = realm
	}
	return auth, true
}

// IngressNginxBasicAuthSecret returns the Secret, as namespace/name, and its
// role for the basic auth of ingress-nginx of the Ingress, if it has one.
func IngressNginxBasicAuthSecret(ing *networking.Ingress) (string, secrets.SecretRole, bool) {
	auth, ok := parseIngressNginxBasicAuth(newIngressNginxAnnotations(ing))
	if !ok || auth.denied != "" {
		return "", "", false
	}
	return auth.secretKey, auth.role, true
}

// parseIngressNginxBasicAuthAnnotations resolves the basic auth of
// ingress-nginx against the Secrets of the Ingress.
func parseIngressNginxBasicAuthAnnotations(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	auth, ok := parseIngressNginxBasicAuth(a)
	if !ok {
		return
	}
	if auth.denied != "" {
		cfgParams.IngressNginxDenied = true
		return
	}

	secretRef := ingEx.SecretRefs[secrets.RefKey(auth.secretKey, auth.role)]
	switch {
	case secretRef == nil || secretRef.Secret == nil:
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: the Secret %s of %s does not exist", ingressNginxDeniedCode, auth.secretKey, IngressNginxAuthSecretAnnotation)
		cfgParams.IngressNginxDenied = true
	case secretRef.Error != nil:
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: the Secret %s of %s is invalid: %v", ingressNginxDeniedCode, auth.secretKey, IngressNginxAuthSecretAnnotation, secretRef.Error)
		cfgParams.IngressNginxDenied = true
	default:
		cfgParams.IngressNginxBasicAuth = &version1.BasicAuth{Secret: secretRef.Path, Realm: auth.realm}
	}
}

// applyIngressNginxAuth puts the basic auth of ingress-nginx on the location,
// or makes it answer 503 if the Ingress is denied. Like ingress-nginx, it
// does not pass the Authorization header of basic auth to the backend.
func applyIngressNginxAuth(loc *version1.Location, cfgParams *ConfigParams) {
	if cfgParams.IngressNginxDenied {
		if loc.PoliciesErrorReturn == nil {
			loc.PoliciesErrorReturn = &version2.Return{Code: ingressNginxDeniedCode}
		}
		return
	}
	loc.AccessLogOff = cfgParams.IngressNginxAccessLogOff
	loc.IngressNginxMirror = cfgParams.IngressNginxMirror
	loc.IngressNginxDirectives = cfgParams.IngressNginxDirectives
	applyIngressNginxMore(loc, cfgParams)
	if cfgParams.IngressNginxProxySSL != nil && loc.EgressMTLS == nil {
		loc.EgressMTLS = cfgParams.IngressNginxProxySSL
	}
	if auth := cfgParams.IngressNginxAuthTLS; auth != nil {
		// as ingress-nginx: what the client certificate check found, and with pass-certificate-to-upstream the certificate
		headers := []version2.Header{{Name: "ssl-client-verify", Value: "$ssl_client_verify"}, {Name: "ssl-client-subject-dn", Value: "$ssl_client_s_dn"}, {Name: "ssl-client-issuer-dn", Value: "$ssl_client_i_dn"}}
		if auth.PassCert {
			headers = append([]version2.Header{{Name: "ssl-client-cert", Value: "$ssl_client_escaped_cert"}}, headers...)
		}
		loc.ProxySetHeaders = append(slices.Clone(loc.ProxySetHeaders), headers...)
	}
	loc.IngressNginxCORS = cfgParams.IngressNginxCORS
	if loc.IngressNginxCORS != nil && loc.AddHeaderInherit == "" {
		// the CORS headers are added in the location; the server's stay
		loc.AddHeaderInherit = addHeaderInheritMerge
	}
	if len(cfgParams.IngressNginxAccess) > 0 {
		loc.AccessRules = cfgParams.IngressNginxAccess
	}
	if cfgParams.IngressNginxBasicAuth != nil {
		loc.BasicAuth = cfgParams.IngressNginxBasicAuth
		loc.ProxySetHeaders = append(slices.Clone(loc.ProxySetHeaders), version2.Header{Name: "Authorization", Value: ""})
	}
}
