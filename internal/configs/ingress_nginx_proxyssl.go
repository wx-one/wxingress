package configs

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/k8s/secrets"
	networking "k8s.io/api/networking/v1"
)

// The annotations of ingress-nginx for TLS to the backends.
const (
	IngressNginxProxySSLSecretAnnotation      = IngressNginxAnnotationPrefix + "proxy-ssl-secret"
	IngressNginxProxySSLVerifyAnnotation      = IngressNginxAnnotationPrefix + "proxy-ssl-verify"
	IngressNginxProxySSLVerifyDepthAnnotation = IngressNginxAnnotationPrefix + "proxy-ssl-verify-depth"
	IngressNginxProxySSLCiphersAnnotation     = IngressNginxAnnotationPrefix + "proxy-ssl-ciphers"
	IngressNginxProxySSLProtocolsAnnotation   = IngressNginxAnnotationPrefix + "proxy-ssl-protocols"
	IngressNginxProxySSLNameAnnotation        = IngressNginxAnnotationPrefix + "proxy-ssl-name"
	IngressNginxProxySSLServerNameAnnotation  = IngressNginxAnnotationPrefix + "proxy-ssl-server-name"
)

var ingressNginxProxySSLAnnotations = []string{
	IngressNginxProxySSLSecretAnnotation, IngressNginxProxySSLVerifyAnnotation, IngressNginxProxySSLVerifyDepthAnnotation,
	IngressNginxProxySSLCiphersAnnotation, IngressNginxProxySSLProtocolsAnnotation, IngressNginxProxySSLNameAnnotation,
	IngressNginxProxySSLServerNameAnnotation,
}

func init() {
	for _, name := range ingressNginxProxySSLAnnotations {
		ingressNginxRead[name] = true
	}
}

var (
	ingressNginxProxySSLProtocolsRegexp = regexp.MustCompile(`^(TLSv1\.2|TLSv1\.3| )*$`)
	ingressNginxProxySSLCiphersRegexp   = regexp.MustCompile(`^[A-Za-z0-9\+:\_\-!]*$`)
	ingressNginxProxySSLNameRegexp      = regexp.MustCompile(`^[A-Za-z0-9.\-_$]+$`)
)

// IngressNginxProxySSLSecret is the Secret (namespace/name) of proxy-ssl-secret, if valid and of the Ingress's namespace.
func IngressNginxProxySSLSecret(ing *networking.Ingress) (string, bool) {
	v := strings.TrimSpace(ing.Annotations[IngressNginxProxySSLSecretAnnotation])
	if v == "" || !ingressNginxAuthTLSSecretRegexp.MatchString(v) {
		return "", false
	}
	ns, name, found := strings.Cut(v, "/")
	if !found {
		ns, name = ing.Namespace, v
	}
	if ns != ing.Namespace {
		return "", false
	}
	return ns + "/" + name, true
}

// parseIngressNginxProxySSL reads proxy-ssl-* with ingress-nginx's defaults
// (verify off, depth 1, TLSv1.2, DEFAULT ciphers, server name off) into the
// TLS to the backends of the Ingress's locations. The Secret gives the client
// certificate (tls.crt, tls.key) and the CA (ca.crt), either or both.
func parseIngressNginxProxySSL(a *ingressNginxAnnotations, ingEx *IngressEx, cfgParams *ConfigParams) {
	set := false
	for _, name := range ingressNginxProxySSLAnnotations {
		if _, ok := a.ing.Annotations[name]; ok {
			set = true
		}
	}
	if !set {
		return
	}
	tls := &version2.EgressMTLS{VerifyDepth: 1, Ciphers: "DEFAULT", Protocols: "TLSv1.2", SessionReuse: true, SSLName: "$proxy_host"}
	if raw, ok := a.value(IngressNginxProxySSLSecretAnnotation); ok {
		key, valid := IngressNginxProxySSLSecret(a.ing)
		if !valid {
			a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: %s %q is not a Secret of the namespace", ingressNginxDeniedCode, IngressNginxProxySSLSecretAnnotation, raw)
			cfgParams.IngressNginxDenied = true
			return
		}
		cert := ingEx.SecretRefs[secrets.RefKey(key, secrets.RoleTLS)]
		ca := ingEx.SecretRefs[secrets.RefKey(key, secrets.RoleCA)]
		if cert != nil && cert.Secret != nil && cert.Error == nil {
			tls.Certificate, tls.CertificateKey = cert.Path, cert.Path
		}
		if ca != nil && ca.Secret != nil && ca.Error == nil {
			tls.TrustedCert = ca.Path
		}
		if tls.Certificate == "" && tls.TrustedCert == "" {
			a.warnings.AddWarningf(a.ing, "the locations of the Ingress answer %d: the Secret %s of %s has neither tls.crt/tls.key nor ca.crt", ingressNginxDeniedCode, key, IngressNginxProxySSLSecretAnnotation)
			cfgParams.IngressNginxDenied = true
			return
		}
	}
	if v, ok := a.onOff(IngressNginxProxySSLVerifyAnnotation); ok {
		tls.VerifyServer = v
	}
	if v, ok := a.value(IngressNginxProxySSLVerifyDepthAnnotation); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			tls.VerifyDepth = n
		} else {
			a.invalid(IngressNginxProxySSLVerifyDepthAnnotation, v, "it must be a number above 0")
		}
	}
	if v, ok := a.value(IngressNginxProxySSLCiphersAnnotation); ok {
		if ingressNginxProxySSLCiphersRegexp.MatchString(v) {
			tls.Ciphers = v
		} else {
			a.invalid(IngressNginxProxySSLCiphersAnnotation, v, "it must be an OpenSSL cipher list")
		}
	}
	if v, ok := a.value(IngressNginxProxySSLProtocolsAnnotation); ok {
		if ingressNginxProxySSLProtocolsRegexp.MatchString(v) && strings.TrimSpace(v) != "" {
			tls.Protocols = strings.Join(strings.Fields(v), " ")
		} else {
			a.invalid(IngressNginxProxySSLProtocolsAnnotation, v, "it must be TLSv1.2 and/or TLSv1.3")
		}
	}
	if v, ok := a.value(IngressNginxProxySSLNameAnnotation); ok {
		if !ingressNginxProxySSLNameRegexp.MatchString(v) {
			a.invalid(IngressNginxProxySSLNameAnnotation, v, "it must be a host name")
		} else if a.variables(IngressNginxProxySSLNameAnnotation, v, false) {
			tls.SSLName = v
		}
	}
	if v, ok := a.onOff(IngressNginxProxySSLServerNameAnnotation); ok {
		tls.ServerName = v
	}
	cfgParams.IngressNginxProxySSL = tls
}
