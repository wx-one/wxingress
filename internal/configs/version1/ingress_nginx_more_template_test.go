package version1

import (
	"strings"
	"testing"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

func TestExecuteTemplate_ForIngressWithIngressNginxConnectionHeader(t *testing.T) {
	t.Parallel()
	upstream := testUpstream
	upstream.Keepalive = "320"
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		for _, loc := range []Location{
			{Path: "/tea", Upstream: upstream, ProxyPass: "http://test", IngressNginxProxyHeaders: true, IngressNginxConnection: "close"},
			{Path: "/tea", Upstream: upstream, ProxyPass: "http://test", Websocket: true, IngressNginxConnection: "close"},
			{Path: "/tea", Upstream: upstream, ProxyPass: "http://test", IngressNginxConnection: "close"},
			{Path: "/tea", Upstream: upstream, ProxyPass: "http://test", ProxyHTTPVersion: "1.0", IngressNginxConnection: "close"},
		} {
			cfg := IngressNginxConfig{
				Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com", Locations: []Location{loc}}},
				Upstreams: []Upstream{upstream},
				Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
			}
			config := executeTemplateNamed(t, tmpl, cfg)
			if n := strings.Count(config, "proxy_set_header Connection"); n != 1 {
				t.Errorf("%s: %d Connection headers, want 1:\n%s", tmpl, n, config)
			}
			requireContains(t, config, "proxy_set_header Connection close;")
			if loc.IngressNginxProxyHeaders {
				requireContains(t, config, "proxy_set_header Upgrade $http_upgrade;")
			}
		}
	}
}

func TestExecuteTemplate_ForIngressWithIngressNginxCustomHeadersAndOtel(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		loc := Location{
			Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test", AddHeaderInherit: "merge",
			IngressNginxCustomHeaders: []version2.Header{{Name: "X-Frame-Options", Value: "DENY"}, {Name: "X-Price", Value: "${literal_dollar}5"}},
			IngressNginxOtel:          &IngressNginxOtel{Trace: true, Context: "propagate", SpanName: "tea"},
		}
		cfg := IngressNginxConfig{
			Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com", Locations: []Location{loc}}},
			Upstreams: []Upstream{testUpstream},
			Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
		}
		config := executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config,
			`proxy_hide_header "X-Frame-Options";`,
			`add_header "X-Frame-Options" "DENY" always;`,
			`add_header "X-Price" "${literal_dollar}5" always;`,
			"add_header_inherit merge;",
			"otel_trace on;", "otel_trace_context propagate;", `otel_span_name "tea";`,
		)
		cfg.Servers[0].Locations[0].IngressNginxOtel = &IngressNginxOtel{}
		config = executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config, "otel_trace off;")
		requireNotContains(t, config, "otel_trace_context")
	}
}

func TestExecuteTemplate_ForIngressWithIngressNginxAuthKeepalive(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		loc := Location{
			Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test",
			IngressNginxExternalAuth: &IngressNginxExternalAuth{
				Location: "/_external-auth-L3RlYQ-Prefix", URL: "http://ingress-nginx-auth_default_cafe_0123456789/auth", Host: "auth.default.svc",
				Upstream:        "ingress-nginx-auth_default_cafe_0123456789",
				ProxySetHeaders: []version2.Header{{Name: "X-Tenant", Value: "$host"}},
			},
		}
		cfg := IngressNginxConfig{
			Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com", Locations: []Location{loc}}},
			Upstreams: []Upstream{testUpstream},
			Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
			IngressNginxAuthUpstreams: []IngressNginxAuthUpstream{{
				Name: "ingress-nginx-auth_default_cafe_0123456789", Server: "auth.default.svc:80", Resolve: true, Resolver: "10.96.0.10 valid=30s",
				Keepalive: 10, KeepaliveRequests: 1000, KeepaliveTimeout: 60,
			}},
		}
		config := executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config,
			"upstream ingress-nginx-auth_default_cafe_0123456789 {",
			"zone ingress-nginx-auth_default_cafe_0123456789 64k;",
			"resolver 10.96.0.10 valid=30s;",
			"server auth.default.svc:80 resolve;",
			"keepalive 10;", "keepalive_requests 1000;", "keepalive_timeout 60s;",
			`proxy_set_header "X-Tenant" "$host";`,
			`set $target "http://ingress-nginx-auth_default_cafe_0123456789/auth";`,
		)
		auth := config[strings.Index(config, "location = /_external-auth"):]
		auth = auth[:strings.Index(auth, "}")]
		requireContains(t, auth, "proxy_http_version 1.1;", `proxy_set_header Connection "";`)
	}
}

func TestExecuteTemplate_ForIngressWithIngressNginxFastCGI(t *testing.T) {
	t.Parallel()
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		loc := Location{
			Path: "/", Upstream: testUpstream, ProxyPass: "http://test",
			IngressNginxFastCGI: &IngressNginxFastCGI{Index: "index.php", Params: []version2.Header{{Name: "SCRIPT_FILENAME", Value: "/var/www$fastcgi_script_name"}}},
		}
		cfg := IngressNginxConfig{
			Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com", Locations: []Location{loc}}},
			Upstreams: []Upstream{testUpstream},
			Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
		}
		config := executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config, "include /etc/nginx/fastcgi_params;", `fastcgi_index "index.php";`,
			`fastcgi_param SCRIPT_FILENAME "/var/www$fastcgi_script_name";`, "fastcgi_pass "+testUpstream.Name+";")
		requireNotContains(t, config, "proxy_pass http://test")
	}
}
