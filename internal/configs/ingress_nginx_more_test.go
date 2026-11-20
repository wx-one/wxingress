package configs

import (
	"context"
	"slices"
	"strings"
	"testing"

	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

func parseIngressNginxMoreForTest(t *testing.T, annotations map[string]string, base ConfigParams, cms map[string]*api_v1.ConfigMap, snippets bool) (ConfigParams, []string) {
	t.Helper()
	ingEx := ingressNginxTestIngress(annotations)
	ingEx.IngressNginxConfigMaps = cms
	cfgParams := parseAnnotations(ingEx, &base, false, false, false, false)
	warnings := parseIngressNginxAnnotations(ingEx, &cfgParams, false, snippets)
	return cfgParams, warnings[ingEx.Ingress]
}

func headersConfigMap(ns string, data map[string]string) *api_v1.ConfigMap {
	return &api_v1.ConfigMap{ObjectMeta: meta_v1.ObjectMeta{Namespace: ns, Name: "headers"}, Data: data}
}

func TestParseIngressNginxConnectionProxyHeader(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	cfg, _ := parseIngressNginxMoreForTest(t, map[string]string{IngressNginxConnectionProxyHeaderAnnotation: "keep-alive"}, *base, nil, false)
	if cfg.IngressNginxConnection != "keep-alive" {
		t.Errorf("connection %q", cfg.IngressNginxConnection)
	}
	cfg, warnings := parseIngressNginxMoreForTest(t, map[string]string{IngressNginxConnectionProxyHeaderAnnotation: "upgrade; evil"}, *base, nil, false)
	if cfg.IngressNginxConnection != "" || !hasWarningContaining(warnings, "connection-proxy-header") {
		t.Errorf("connection %q, warnings %v", cfg.IngressNginxConnection, warnings)
	}
}

func TestParseIngressNginxCustomHeaders(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	base.IngressNginxAllowedResponseHeaders = []string{"X-Frame-Options", "x-price"}
	cms := map[string]*api_v1.ConfigMap{"default/headers": headersConfigMap("default", map[string]string{"X-Frame-Options": "DENY", "X-Price": "$5"})}
	ann := map[string]string{IngressNginxCustomHeadersAnnotation: "default/headers"}

	cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, cms, false)
	want := []version2.Header{{Name: "X-Frame-Options", Value: "DENY"}, {Name: "X-Price", Value: "${literal_dollar}5"}}
	if cfg.IngressNginxDenied || !slices.Equal(cfg.IngressNginxCustomHeaders, want) {
		t.Errorf("headers %v, denied %v, warnings %v", cfg.IngressNginxCustomHeaders, cfg.IngressNginxDenied, warnings)
	}
	// a name without namespace is one of the Ingress's
	cfg, _ = parseIngressNginxMoreForTest(t, map[string]string{IngressNginxCustomHeadersAnnotation: "headers"}, *base, cms, false)
	if cfg.IngressNginxDenied || len(cfg.IngressNginxCustomHeaders) != 2 {
		t.Errorf("a bare name: headers %v, denied %v", cfg.IngressNginxCustomHeaders, cfg.IngressNginxDenied)
	}

	for _, tc := range []struct {
		name    string
		ann     string
		allowed []string
		cms     map[string]*api_v1.ConfigMap
		warning string
	}{
		{name: "missing", ann: "default/headers", allowed: base.IngressNginxAllowedResponseHeaders, warning: "does not exist"},
		{name: "another namespace", ann: "other/headers", allowed: base.IngressNginxAllowedResponseHeaders,
			cms: map[string]*api_v1.ConfigMap{"other/headers": headersConfigMap("other", map[string]string{"X-Frame-Options": "DENY"})}, warning: "another namespace"},
		{name: "not allowed", ann: "default/headers", allowed: []string{"X-Frame-Options"}, cms: cms, warning: "global-allowed-response-headers"},
		{name: "invalid value", ann: "default/headers", allowed: base.IngressNginxAllowedResponseHeaders,
			cms: map[string]*api_v1.ConfigMap{"default/headers": headersConfigMap("default", map[string]string{"X-Frame-Options": "a\nb"})}, warning: "invalid value"},
	} {
		params := *base
		params.IngressNginxAllowedResponseHeaders = tc.allowed
		cfg, warnings := parseIngressNginxMoreForTest(t, map[string]string{IngressNginxCustomHeadersAnnotation: tc.ann}, params, tc.cms, false)
		if !cfg.IngressNginxDenied || !hasWarningContaining(warnings, tc.warning) {
			t.Errorf("%s: denied %v, warnings %v", tc.name, cfg.IngressNginxDenied, warnings)
		}
	}
}

func TestIngressNginxConfigMapRefs(t *testing.T) {
	t.Parallel()
	ing := ingressNginxTestIngress(map[string]string{
		IngressNginxCustomHeadersAnnotation:       "headers",
		IngressNginxAuthProxySetHeadersAnnotation: "default/auth-headers",
	}).Ingress
	if got := IngressNginxConfigMapRefs(ing); !slices.Equal(got, []string{"default/headers", "default/auth-headers"}) {
		t.Errorf("refs %v", got)
	}
	ing.Annotations[IngressNginxCustomHeadersAnnotation] = "kube-system/secret-stuff"
	if got := IngressNginxConfigMapRefs(ing); !slices.Equal(got, []string{"default/auth-headers"}) {
		t.Errorf("refs of another namespace are not watched: %v", got)
	}
}

func TestParseIngressNginxOtel(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	ann := map[string]string{IngressNginxEnableOpentelemetryAnnotation: "true", IngressNginxOtelOperationNameAnnotation: "tea"}
	cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, nil, false)
	if cfg.IngressNginxOtel != nil || !hasWarningContaining(warnings, "otel-exporter-endpoint") {
		t.Errorf("without the module: %+v, %v", cfg.IngressNginxOtel, warnings)
	}
	base.MainOtelLoadModule = true
	cfg, _ = parseIngressNginxMoreForTest(t, ann, *base, nil, false)
	if o := cfg.IngressNginxOtel; o == nil || !o.Trace || o.Context != "propagate" || o.SpanName != "tea" {
		t.Errorf("otel %+v", o)
	}
	ann[IngressNginxOtelTrustIncomingSpanAnnotation] = "false"
	cfg, _ = parseIngressNginxMoreForTest(t, ann, *base, nil, false)
	if o := cfg.IngressNginxOtel; o == nil || o.Context != "inject" {
		t.Errorf("not trusted: %+v", o)
	}
	base.MainOtelTraceInHTTP = true
	cfg, _ = parseIngressNginxMoreForTest(t, map[string]string{IngressNginxEnableOpentelemetryAnnotation: "false"}, *base, nil, false)
	if o := cfg.IngressNginxOtel; o == nil || o.Trace {
		t.Errorf("off where tracing is on: %+v", o)
	}
}

func TestParseIngressNginxStreamSnippet(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	ann := map[string]string{IngressNginxStreamSnippetAnnotation: "server {\n  listen 8000;\n  proxy_pass x:1;\n}"}
	cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, nil, false)
	if cfg.IngressNginxStreamSnippet != nil || !hasWarningContaining(warnings, "snippets are not enabled") {
		t.Errorf("without snippets: %v %v", cfg.IngressNginxStreamSnippet, warnings)
	}
	cfg, _ = parseIngressNginxMoreForTest(t, ann, *base, nil, true)
	if len(cfg.IngressNginxStreamSnippet) != 4 {
		t.Errorf("snippet %q", cfg.IngressNginxStreamSnippet)
	}
	if got := uniqueStreamSnippets(cfg.IngressNginxStreamSnippet, nil, cfg.IngressNginxStreamSnippet); len(got) != 1 {
		t.Errorf("unique %q", got)
	}
}

func TestParseIngressNginxAuthKeepaliveAndProxySetHeaders(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	base.HTTP2 = false
	cms := map[string]*api_v1.ConfigMap{"default/headers": headersConfigMap("default", map[string]string{"X-Tenant": "$host"})}
	ann := map[string]string{
		IngressNginxAuthURLAnnotation: "http://auth.default.svc.cluster.local:4180/oauth2/auth?x=1", IngressNginxAuthKeepaliveAnnotation: "10",
		IngressNginxAuthKeepaliveTimeoutAnnotation: "30", IngressNginxAuthProxySetHeadersAnnotation: "headers",
		IngressNginxAuthKeepaliveShareVarsAnnotation: "true",
	}
	cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, cms, false)
	auth := cfg.IngressNginxExternalAuth
	if auth == nil || auth.upstream == nil {
		t.Fatalf("auth %+v, warnings %v", auth, warnings)
	}
	u := auth.upstream
	if u.Server != "auth.default.svc.cluster.local:4180" || !u.Resolve || u.Keepalive != 10 || u.KeepaliveTimeout != 30 || u.KeepaliveRequests != 1000 {
		t.Errorf("upstream %+v", u)
	}
	if !strings.HasPrefix(auth.URL, "http://"+u.Name+"/oauth2/auth?x=1") || auth.Upstream != u.Name {
		t.Errorf("url %q upstream %q", auth.URL, auth.Upstream)
	}
	if !slices.Equal(auth.ProxySetHeaders, []version2.Header{{Name: "X-Tenant", Value: "$host"}}) {
		t.Errorf("headers %v", auth.ProxySetHeaders)
	}
	if got := ingressNginxAuthUpstreamsOf(auth, "10.0.0.1 valid=30s"); len(got) != 1 || got[0].Resolver != "10.0.0.1 valid=30s" {
		t.Errorf("upstreams %+v", got)
	}
	if hasWarningContaining(warnings, "not supported") {
		t.Errorf("warnings %v", warnings)
	}

	// with HTTP/2, as ingress-nginx, without keepalive
	base.HTTP2 = true
	cfg, warnings = parseIngressNginxMoreForTest(t, ann, *base, cms, false)
	if cfg.IngressNginxExternalAuth == nil || cfg.IngressNginxExternalAuth.upstream != nil || !hasWarningContaining(warnings, "HTTP/2") {
		t.Errorf("http2: %+v %v", cfg.IngressNginxExternalAuth, warnings)
	}
}

func TestParseIngressNginxFastCGI(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	cms := map[string]*api_v1.ConfigMap{"default/fcgi": {Data: map[string]string{"SCRIPT_FILENAME": "/var/www/html$fastcgi_script_name", "HTTP_PROXY": ""}}}
	ann := map[string]string{IngressNginxBackendProtocolAnnotation: "FCGI", IngressNginxFastCGIIndexAnnotation: "index.php", IngressNginxFastCGIParamsAnnotation: "fcgi"}
	cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, cms, false)
	f := cfg.IngressNginxFastCGI
	if cfg.IngressNginxDenied || f == nil || f.Index != "index.php" ||
		!slices.Equal(f.Params, []version2.Header{{Name: "HTTP_PROXY", Value: ""}, {Name: "SCRIPT_FILENAME", Value: "/var/www/html$fastcgi_script_name"}}) {
		t.Errorf("fastcgi %+v, denied %v, warnings %v", f, cfg.IngressNginxDenied, warnings)
	}
	cms["default/fcgi"].Data["X"] = "a b"
	cfg, warnings = parseIngressNginxMoreForTest(t, ann, *base, cms, false)
	if !cfg.IngressNginxDenied || !hasWarningContaining(warnings, "parameter X is invalid") {
		t.Errorf("invalid: denied %v, warnings %v", cfg.IngressNginxDenied, warnings)
	}
	cfg, warnings = parseIngressNginxMoreForTest(t, map[string]string{IngressNginxFastCGIIndexAnnotation: "index.php"}, *base, nil, false)
	if cfg.IngressNginxFastCGI != nil || !hasWarningContaining(warnings, "is not FCGI") {
		t.Errorf("without FCGI: %+v %v", cfg.IngressNginxFastCGI, warnings)
	}
}
