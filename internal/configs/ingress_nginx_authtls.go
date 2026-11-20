package configs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	networking "k8s.io/api/networking/v1"
)

// The client certificate annotations of ingress-nginx.
const (
	IngressNginxAuthTLSSecretAnnotation          = IngressNginxAnnotationPrefix + "auth-tls-secret"
	IngressNginxAuthTLSVerifyClientAnnotation    = IngressNginxAnnotationPrefix + "auth-tls-verify-client"
	IngressNginxAuthTLSVerifyDepthAnnotation     = IngressNginxAnnotationPrefix + "auth-tls-verify-depth"
	IngressNginxAuthTLSErrorPageAnnotation       = IngressNginxAnnotationPrefix + "auth-tls-error-page"
	IngressNginxAuthTLSPassCertificateAnnotation = IngressNginxAnnotationPrefix + "auth-tls-pass-certificate-to-upstream"
	IngressNginxAuthTLSMatchCNAnnotation         = IngressNginxAnnotationPrefix + "auth-tls-match-cn"
)

func init() {
	for _, name := range []string{
		IngressNginxAuthTLSSecretAnnotation, IngressNginxAuthTLSVerifyClientAnnotation, IngressNginxAuthTLSVerifyDepthAnnotation,
		IngressNginxAuthTLSErrorPageAnnotation, IngressNginxAuthTLSPassCertificateAnnotation, IngressNginxAuthTLSMatchCNAnnotation,
	} {
		ingressNginxRead[name] = true
	}
}

var (
	ingressNginxAuthTLSVerifyRegexp   = regexp.MustCompile(`^(on|off|optional|optional_no_ca)$`)
	ingressNginxAuthTLSErrorRegexp    = regexp.MustCompile(`^(@[A-Za-z0-9_-]+|((https?://)?[A-Za-z0-9\-.]+(:\d+)?)?(/[A-Za-z0-9\-_.]+)*/?)$`)
	ingressNginxAuthTLSSecretRegexp   = regexp.MustCompile(`^([a-z0-9]([-a-z0-9]*[a-z0-9])?/)?[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	ingressNginxAuthTLSMatchCNInvalid = regexp.MustCompile(`["\\\n]`)
)

// ingressNginxAuthTLS is what the auth-tls annotations of an Ingress ask for.
type ingressNginxAuthTLS struct {
	secretKey    string
	verifyClient string
	verifyDepth  int
	errorPage    string
	passCert     bool
	matchCN      string
	denied       string
}

// IngressNginxAuthTLSSecret returns the CA Secret (namespace/name) of auth-tls-secret, if the Ingress has a valid one.
func IngressNginxAuthTLSSecret(ing *networking.Ingress) (string, bool) {
	auth, ok := parseIngressNginxAuthTLS(newIngressNginxAnnotations(ing))
	if !ok || auth.denied != "" {
		return "", false
	}
	return auth.secretKey, true
}

// parseIngressNginxAuthTLS reads the auth-tls annotations as ingress-nginx
// does: invalid optional ones are their defaults (on, depth 1), a Secret that
// cannot be had denies the locations - here also one of another namespace,
// as ingress-nginx refuses it without allow-cross-namespace-resources.
func parseIngressNginxAuthTLS(a *ingressNginxAnnotations) (ingressNginxAuthTLS, bool) {
	secret, exists := a.value(IngressNginxAuthTLSSecretAnnotation)
	if !exists {
		return ingressNginxAuthTLS{}, false
	}
	auth := ingressNginxAuthTLS{verifyClient: "on", verifyDepth: 1}
	if !ingressNginxAuthTLSSecretRegexp.MatchString(secret) {
		auth.denied = "auth-tls-secret " + strconv.Quote(secret) + " is not <namespace>/<name>"
		return auth, true
	}
	ns, name, found := strings.Cut(secret, "/")
	if !found {
		ns, name = a.ing.Namespace, secret
	}
	if ns != a.ing.Namespace {
		auth.denied = "auth-tls-secret " + strconv.Quote(secret) + " is in another namespace"
		return auth, true
	}
	auth.secretKey = ns + "/" + name
	if v, ok := a.value(IngressNginxAuthTLSVerifyClientAnnotation); ok {
		if ingressNginxAuthTLSVerifyRegexp.MatchString(v) {
			auth.verifyClient = v
		} else {
			a.invalid(IngressNginxAuthTLSVerifyClientAnnotation, v, "it must be on, off, optional or optional_no_ca; on is taken")
		}
	}
	if v, ok := a.value(IngressNginxAuthTLSVerifyDepthAnnotation); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			auth.verifyDepth = n
		} else {
			a.invalid(IngressNginxAuthTLSVerifyDepthAnnotation, v, "it must be a number above 0; 1 is taken")
		}
	}
	if v, ok := a.value(IngressNginxAuthTLSErrorPageAnnotation); ok {
		if ingressNginxAuthTLSErrorRegexp.MatchString(v) {
			auth.errorPage = v
		} else {
			a.invalid(IngressNginxAuthTLSErrorPageAnnotation, v, "it must be a URL or a named location")
		}
	}
	if v, ok := a.boolean(IngressNginxAuthTLSPassCertificateAnnotation); ok {
		auth.passCert = v
	}
	if v, ok := a.value(IngressNginxAuthTLSMatchCNAnnotation); ok {
		re := strings.TrimPrefix(v, "CN=")
		if _, err := regexp.Compile(re); err != nil || !strings.HasPrefix(v, "CN=") || ingressNginxAuthTLSMatchCNInvalid.MatchString(v) {
			auth.denied = "auth-tls-match-cn " + strconv.Quote(v) + " is not CN=<regular expression>"
			return auth, true
		}
		auth.matchCN = v
	}
	return auth, true
}

// parseIngressNginxAuthTLSAnnotations resolves the client certificate auth
// against the CA Secret of the Ingress into cfgParams.
func parseIngressNginxAuthTLSAnnotations(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	auth, ok := parseIngressNginxAuthTLS(a)
	if !ok {
		return
	}
	if auth.denied != "" {
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s", ingressNginxDeniedCode, auth.denied)
		cfgParams.IngressNginxDenied = true
		return
	}
	ref := ingEx.SecretRefs[secrets.RefKey(auth.secretKey, secrets.RoleCA)]
	switch {
	case ref == nil || ref.Secret == nil:
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: the Secret %s of %s does not exist", ingressNginxDeniedCode, auth.secretKey, IngressNginxAuthTLSSecretAnnotation)
		cfgParams.IngressNginxDenied = true
		return
	case ref.Error != nil:
		a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: the Secret %s of %s is invalid: %v", ingressNginxDeniedCode, auth.secretKey, IngressNginxAuthTLSSecretAnnotation, ref.Error)
		cfgParams.IngressNginxDenied = true
		return
	}
	cfgParams.IngressNginxAuthTLS = &IngressNginxClientCert{
		MTLS:      &version2.IngressMTLS{ClientCert: ref.Path, ClientCrl: ref.CRLPath, VerifyClient: auth.verifyClient, VerifyDepth: auth.verifyDepth},
		ErrorPage: auth.errorPage,
		PassCert:  auth.passCert,
		MatchCN:   auth.matchCN,
	}
}

// IngressNginxClientCert is the client certificate auth of an Ingress.
type IngressNginxClientCert struct {
	MTLS      *version2.IngressMTLS
	ErrorPage string
	PassCert  bool
	MatchCN   string
}
