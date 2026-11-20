package configs

import (
	"context"
	"slices"
	"strings"
	"testing"

	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func aliasIngress(annotations map[string]string) *IngressEx {
	backend := func(name string) networking.IngressBackend {
		return networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: name, Port: networking.ServiceBackendPort{Number: 80}}}
	}
	return &IngressEx{Ingress: &networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ing", Annotations: annotations},
		Spec: networking.IngressSpec{Rules: []networking.IngressRule{{Host: "a.test", IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{
			Paths: []networking.HTTPIngressPath{{Path: "/", Backend: backend("web")}, {Path: "/api", Backend: backend("api")}, {Path: "/x", Backend: backend("web")}},
		}}}}},
	}}
}

func TestWithIngressNginxAliases(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]string
		warning     string
	}{
		{"upstream-vhost", map[string]string{p + "upstream-vhost": "backend.internal"}, map[string]string{UpstreamVhostAnnotation: "backend.internal"}, ""},
		{"an injection is not taken", map[string]string{p + "upstream-vhost": "x; return 200"}, map[string]string{UpstreamVhostAnnotation: ""}, "is invalid"},
		{"nginx.org wins", map[string]string{p + "app-root": "/app", "nginx.org/app-root": "/other"}, map[string]string{"nginx.org/app-root": "/other"}, "takes precedence"},
		{"app-root", map[string]string{p + "app-root": "/app1"}, map[string]string{"nginx.org/app-root": "/app1"}, ""},
		{"ssl-redirect", map[string]string{p + "ssl-redirect": "false"}, map[string]string{SSLRedirectAnnotation: "false"}, ""},
		{"backend-protocol HTTPS", map[string]string{p + "backend-protocol": "HTTPS"}, map[string]string{"nginx.org/ssl-services": "api,web", "nginx.org/grpc-services": ""}, ""},
		{"backend-protocol GRPCS", map[string]string{p + "backend-protocol": "grpcs"}, map[string]string{"nginx.org/ssl-services": "api,web", "nginx.org/grpc-services": "api,web"}, ""},
		{"backend-protocol AJP denies", map[string]string{p + "backend-protocol": "AJP"}, map[string]string{ingressNginxDeniedAnnotation: "true"}, "answer 503"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			in := aliasIngress(tt.annotations)
			out, warnings := withIngressNginxAliases(in)
			for k, v := range tt.want {
				if got := out.Ingress.Annotations[k]; got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			if len(in.Ingress.Annotations) != len(tt.annotations) {
				t.Error("the annotations of the Ingress itself were changed")
			}
			all := strings.Join(warnings[in.Ingress], "\n")
			if tt.warning != "" && !strings.Contains(all, tt.warning) {
				t.Errorf("no warning %q in %q", tt.warning, all)
			}
			if tt.warning == "" && all != "" {
				t.Errorf("warnings %q", all)
			}
		})
	}
}

func TestIngressNginxServerAnnotations(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: map[string]string{
		p + "force-ssl-redirect": "true", p + "preserve-trailing-slash": "true", p + "enable-access-log": "false",
		p + "server-alias": "b.test, *.c.test bad;name",
	}}}
	a := newIngressNginxAnnotations(ing)
	cfg := &ConfigParams{}
	parseIngressNginxServerAnnotations(a, cfg)
	if !cfg.IngressNginxForceSSLRedirect || !cfg.IngressNginxPreserveTrailingSlash || !cfg.IngressNginxAccessLogOff {
		t.Errorf("%+v", cfg)
	}
	if !slices.Equal(cfg.IngressNginxServerAliases, []string{"b.test", "*.c.test"}) {
		t.Errorf("aliases %q", cfg.IngressNginxServerAliases)
	}
	if len(a.warnings[ing]) != 1 {
		t.Errorf("warnings %q", a.warnings[ing])
	}
}

func TestGenerateNginxCfgIngressNginxRedirect(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxForceSSLRedirectAnnotation] = "true"
	ingEx.Ingress.Annotations[IngressNginxServerAliasAnnotation] = "alias.example.com"
	ingEx.Ingress.Annotations[IngressNginxEnableAccessLogAnnotation] = "false"
	result, _ := generateNginxCfg(NginxCfgParams{
		staticParams:  &StaticConfigParams{},
		ingEx:         &ingEx,
		BaseCfgParams: NewDefaultConfigParamsFor(context.Background(), false, true),
	})
	s := result.Servers[0]
	if !s.ForceSSLRedirect || !s.IngressNginxRedirect || !slices.Equal(s.ServerAliases, []string{"alias.example.com"}) {
		t.Errorf("server %+v", s)
	}
	if !s.Locations[0].AccessLogOff {
		t.Error("access log of the location is on")
	}
}

func TestParseIngressNginxCORS(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	parse := func(annotations map[string]string) (*ConfigParams, []string) {
		ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: annotations}}
		a := newIngressNginxAnnotations(ing)
		return &ConfigParams{IngressNginxCORS: parseIngressNginxCORS(a)}, a.warnings[ing]
	}
	if cfg, _ := parse(map[string]string{p + "cors-allow-origin": "https://a.test"}); cfg.IngressNginxCORS != nil {
		t.Error("CORS without enable-cors")
	}
	cfg, _ := parse(map[string]string{p + "enable-cors": "true"})
	c := cfg.IngressNginxCORS
	if c == nil || !c.AnyOrigin || c.Methods != ingressNginxCORSMethods || !c.Credentials || c.MaxAge != 1728000 {
		t.Errorf("defaults %+v", c)
	}
	cfg, warnings := parse(map[string]string{
		p + "enable-cors": "true", p + "cors-allow-origin": "https://a.test, https://*.b.test:8443, bad origin",
		p + "cors-allow-methods": "GET, POST", p + "cors-allow-headers": "X-A,X-B", p + "cors-allow-credentials": "false",
		p + "cors-expose-headers": "X-Total", p + "cors-max-age": "600",
	})
	c = cfg.IngressNginxCORS
	if c.AnyOrigin || c.OriginRegex != `((https://a\.test)|(https://[A-Za-z0-9\-]+\.b\.test:8443))$` {
		t.Errorf("origin regex %q", c.OriginRegex)
	}
	if c.Methods != "GET, POST" || c.Headers != "X-A,X-B" || c.Credentials || c.ExposeHeaders != "X-Total" || c.MaxAge != 600 {
		t.Errorf("%+v", c)
	}
	if len(warnings) != 1 {
		t.Errorf("warnings %q", warnings)
	}
	cfg, warnings = parse(map[string]string{p + "enable-cors": "true", p + "cors-allow-headers": "X-A\"; return 200"})
	if cfg.IngressNginxCORS.Headers != ingressNginxCORSHeaders || len(warnings) != 1 {
		t.Errorf("an injection is taken: %+v %q", cfg.IngressNginxCORS, warnings)
	}
}

func TestParseIngressNginxAuthTLS(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		denied      bool
		want        ingressNginxAuthTLS
	}{
		{"defaults", map[string]string{p + "auth-tls-secret": "default/ca"}, false, ingressNginxAuthTLS{secretKey: "default/ca", verifyClient: "on", verifyDepth: 1}},
		{"name only: the Ingress's namespace", map[string]string{p + "auth-tls-secret": "ca"}, false, ingressNginxAuthTLS{secretKey: "default/ca", verifyClient: "on", verifyDepth: 1}},
		{"another namespace denies", map[string]string{p + "auth-tls-secret": "other/ca"}, true, ingressNginxAuthTLS{}},
		{"all of them", map[string]string{p + "auth-tls-secret": "default/ca", p + "auth-tls-verify-client": "optional", p + "auth-tls-verify-depth": "3", p + "auth-tls-error-page": "https://err.test/cert", p + "auth-tls-pass-certificate-to-upstream": "true", p + "auth-tls-match-cn": "CN=(alice|bob)"}, false,
			ingressNginxAuthTLS{secretKey: "default/ca", verifyClient: "optional", verifyDepth: 3, errorPage: "https://err.test/cert", passCert: true, matchCN: "CN=(alice|bob)"}},
		{"invalid verify-client is on", map[string]string{p + "auth-tls-secret": "default/ca", p + "auth-tls-verify-client": "maybe"}, false, ingressNginxAuthTLS{secretKey: "default/ca", verifyClient: "on", verifyDepth: 1}},
		{"an error page that breaks out is left out", map[string]string{p + "auth-tls-secret": "default/ca", p + "auth-tls-error-page": "/x; return 200"}, false, ingressNginxAuthTLS{secretKey: "default/ca", verifyClient: "on", verifyDepth: 1}},
		{"a match-cn with a quote denies", map[string]string{p + "auth-tls-secret": "default/ca", p + "auth-tls-match-cn": `CN=a"`}, true, ingressNginxAuthTLS{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ing", Annotations: tt.annotations}}
			got, ok := parseIngressNginxAuthTLS(newIngressNginxAnnotations(ing))
			if !ok || (got.denied != "") != tt.denied {
				t.Fatalf("ok %v, denied %q", ok, got.denied)
			}
			if !tt.denied && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestParseIngressNginxDirectives(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: map[string]string{
		p + "enable-rewrite-log": "true", p + "proxy-cookie-path": "/ /app", p + "proxy-cookie-domain": "a.test b.test; return 200",
		p + "relative-redirects": "true", p + "use-port-in-redirects": "true", p + "satisfy": "any",
		p + "custom-http-errors": "404", p + "disable-proxy-intercept-errors": "true",
	}}}
	a := newIngressNginxAnnotations(ing)
	cfg := &ConfigParams{IngressNginxErrorCodes: []int{404}}
	parseIngressNginxDirectives(a, cfg)
	want := []string{"absolute_redirect off", "port_in_redirect on", "proxy_cookie_path / /app", "rewrite_log on", "satisfy any"}
	if !slices.Equal(cfg.IngressNginxDirectives, want) {
		t.Errorf("got %q, want %q", cfg.IngressNginxDirectives, want)
	}
	if cfg.IngressNginxErrorCodes != nil || len(a.warnings[ing]) != 1 {
		t.Errorf("error codes %v, warnings %q", cfg.IngressNginxErrorCodes, a.warnings[ing])
	}
}
