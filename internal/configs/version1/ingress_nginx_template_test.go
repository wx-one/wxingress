package version1

import (
	"bytes"
	"strings"
	"testing"
	"text/template"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

// executeIngressTemplateForLocations renders an Ingress config of one server with the locations.
func executeIngressTemplateForLocations(t *testing.T, cfg IngressNginxConfig, locations ...Location) string {
	t.Helper()
	if len(cfg.Servers) == 0 {
		cfg.Servers = []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com"}}
	}
	cfg.Servers[0].Locations = locations
	if len(cfg.Upstreams) == 0 {
		cfg.Upstreams = []Upstream{testUpstream}
	}
	cfg.Ingress = Ingress{Name: "cafe-ingress", Namespace: "default"}

	buf := &bytes.Buffer{}
	if err := newNGINXIngressTmpl(t).Execute(buf, cfg); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func requireContains(t *testing.T, config string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(config, want) {
			t.Errorf("the config lacks %q:\n%s", want, config)
		}
	}
}

func requireNotContains(t *testing.T, config string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(config, u) {
			t.Errorf("the config has %q:\n%s", u, config)
		}
	}
}

func TestExecuteTemplate_ForIngressWithIngressNginxRegex(t *testing.T) {
	t.Parallel()
	config := executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{Path: "/tea(/|$)(.*)", Upstream: testUpstream, ProxyPass: "http://test", RewriteTarget: "/$2", UseRegex: true, CaseInsensitiveRegex: true, CaseInsensitiveRewrite: true},
		Location{Path: "= /coffee", Upstream: testUpstream, ProxyPass: "http://test", CaseInsensitiveRegex: true},
		Location{Path: `/a"b\c`, Upstream: testUpstream, ProxyPass: "http://test", CaseInsensitiveRegex: true},
	)
	requireContains(t, config,
		`location ~* "^/tea(/|$)(.*)" {`,
		`rewrite "(?i)/tea(/|$)(.*)" /$2 break;`,
		`location ~* "^/coffee" {`,
		`location ~* "^/a\"b\\c" {`,
	)
}

func TestExecuteTemplate_ForIngressWithIngressNginxRateLimit(t *testing.T) {
	t.Parallel()
	rl := IngressNginxRateLimit{
		Name: "default_cafe_uid", ID: "ZGVmYXVsdF9jYWZlX3VpZA", Key: "$binary_remote_addr",
		Allowlist:   []string{"10.0.0.0/8", "192.168.1.1"},
		Connections: 3, RPS: 10, RPSBurst: 50, RPM: 100, RPMBurst: 500, ZoneSize: "5m",
	}
	cfg := IngressNginxConfig{IngressNginxRateLimits: []IngressNginxRateLimit{rl}}
	config := executeIngressTemplateForLocations(t, cfg,
		Location{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test", IngressNginxRateLimit: &rl, LimitRate: "100k", LimitRateAfter: "1k"})
	requireContains(t, config,
		"geo $remote_addr $allowlist_ZGVmYXVsdF9jYWZlX3VpZA {\n\tdefault 0;\n\t10.0.0.0/8 1;\n\t192.168.1.1 1;\n}",
		"map $allowlist_ZGVmYXVsdF9jYWZlX3VpZA $limit_ZGVmYXVsdF9jYWZlX3VpZA {\n\t0 $binary_remote_addr;\n\t1 \"\";\n}",
		"limit_conn_zone $limit_ZGVmYXVsdF9jYWZlX3VpZA zone=default_cafe_uid_conn:5m;",
		"limit_req_zone $limit_ZGVmYXVsdF9jYWZlX3VpZA zone=default_cafe_uid_rps:5m rate=10r/s;",
		"limit_req_zone $limit_ZGVmYXVsdF9jYWZlX3VpZA zone=default_cafe_uid_rpm:5m rate=100r/m;",
		"limit_conn default_cafe_uid_conn 3;",
		"limit_conn_status 503;",
		"limit_req zone=default_cafe_uid_rps burst=50 nodelay;",
		"limit_req zone=default_cafe_uid_rpm burst=500 nodelay;",
		"limit_req_status 503;",
		"limit_rate_after 1k;",
		"limit_rate 100k;",
	)

	onlyRPS := IngressNginxRateLimit{Name: "n", ID: "id", Key: "$binary_remote_addr", RPS: 1, RPSBurst: 5, ZoneSize: "5m"}
	config = executeIngressTemplateForLocations(t, IngressNginxConfig{IngressNginxRateLimits: []IngressNginxRateLimit{onlyRPS}},
		Location{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test", IngressNginxRateLimit: &onlyRPS})
	requireNotContains(t, config, "limit_conn", "rpm", "limit_rate")
}

func TestExecuteTemplate_ForIngressWithIngressNginxBasicAuth(t *testing.T) {
	t.Parallel()
	config := executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{
			Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test",
			BasicAuth:       &BasicAuth{Secret: "/etc/nginx/secrets/basic_auth_default_users_auth-map", Realm: "Authentication Required"},
			ProxySetHeaders: []version2.Header{{Name: "Authorization", Value: ""}},
		},
		Location{Path: "/coffee", Upstream: testUpstream, ProxyPass: "http://test", PoliciesErrorReturn: &version2.Return{Code: 503}},
	)
	requireContains(t, config,
		`auth_basic "Authentication Required";`,
		"auth_basic_user_file /etc/nginx/secrets/basic_auth_default_users_auth-map;",
		`proxy_set_header Authorization "";`,
		"return 503;",
	)
}

func TestExecuteTemplate_ForIngressWithIngressNginxExternalAuth(t *testing.T) {
	t.Parallel()
	auth := &IngressNginxExternalAuth{
		Location:        "/_external-auth-L3RlYQ-Prefix",
		URL:             "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth",
		Host:            "oauth2-proxy.auth.svc.cluster.local",
		ResponseHeaders: []IngressNginxAuthHeader{{Name: "X-Auth-Request-User", Variable: "$upstream_http_x_auth_request_user"}},
		SigninLocation:  "@0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3",
		SigninURL:       "https://auth.example.com/start?rd=$pass_access_scheme://$http_host$escaped_request_uri",
		CacheZone:       "auth_cache_default_cafe",
		CacheKey:        "cafe.example.com/_external-auth-L3RlYQ-Prefix$remote_user",
		CacheDurations:  []string{"200 202 401 5m"},
	}
	cfg := IngressNginxConfig{IngressNginxAuthCaches: []IngressNginxAuthCache{{Zone: "auth_cache_default_cafe", Path: "/var/cache/nginx/auth_cache_default_cafe"}}}
	config := executeIngressTemplateForLocations(t, cfg,
		Location{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test", ClientMaxBodySize: "1m", IngressNginxExternalAuth: auth, AddHeaderInherit: "merge"})
	requireContains(t, config,
		"proxy_cache_path /var/cache/nginx/auth_cache_default_cafe levels=1:2 keys_zone=auth_cache_default_cafe:10m max_size=128m inactive=30m use_temp_path=off;",
		"location = /_external-auth-L3RlYQ-Prefix {\n\t\tinternal;",
		"proxy_cache auth_cache_default_cafe;",
		"proxy_cache_valid 200 202 401 5m;",
		`proxy_cache_key "cafe.example.com/_external-auth-L3RlYQ-Prefix$remote_user";`,
		"proxy_pass_request_body off;",
		"proxy_set_header X-Request-ID $ingress_nginx_req_id;",
		"proxy_set_header Host oauth2-proxy.auth.svc.cluster.local;",
		"proxy_set_header X-Original-URL $scheme://$http_host$request_uri;",
		"proxy_set_header X-Auth-Request-Redirect $request_uri;",
		"proxy_buffering on;",
		"proxy_http_version 1.1;",
		`set $target "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth";`,
		"proxy_pass $target;",
		"location @0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3 {\n\t\tinternal;\n\t\tadd_header Set-Cookie $auth_cookie;\n\t\treturn 302 https://auth.example.com/start?rd=$pass_access_scheme://$http_host$escaped_request_uri;\n\t}",
		"auth_request /_external-auth-L3RlYQ-Prefix;",
		"auth_request_set $auth_cookie $upstream_http_set_cookie;",
		"add_header Set-Cookie $auth_cookie;",
		"auth_request_set $authHeader0 $upstream_http_x_auth_request_user;",
		"proxy_set_header 'X-Auth-Request-User' $authHeader0;",
		"error_page 401 = @0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3;",
		"add_header_inherit merge;",
	)
	requireNotContains(t, config, "proxy_method", "resolver")

	dynamic := &IngressNginxExternalAuth{Location: "/_external-auth-Lw-default", URL: "http://$host/auth", Resolver: "10.96.0.10 valid=30s", Host: "$host", Method: "POST", AlwaysSetCookie: true}
	config = executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{Path: "/", Upstream: testUpstream, ProxyPass: "http://test", IngressNginxExternalAuth: dynamic})
	requireContains(t, config,
		"resolver 10.96.0.10 valid=30s;",
		`set $target "http://$host/auth";`,
		"proxy_pass $target;",
		"proxy_method POST;",
		"proxy_set_header X-Original-URI $request_uri;",
		"add_header Set-Cookie $auth_cookie always;",
		"proxy_buffering off;",
	)
	requireNotContains(t, config, "error_page 401", "proxy_cache")
}

func TestExecuteTemplate_ForIngressWithProxyRequestBuffering(t *testing.T) {
	t.Parallel()
	config := executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test", ProxyRequestBuffering: "off", ProxyBuffers: "4 16k", ProxyBufferSize: "16k"})
	requireContains(t, config, "proxy_request_buffering off;", "proxy_buffers 4 16k;", "proxy_buffer_size 16k;")

	config = executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test"})
	requireNotContains(t, config, "proxy_request_buffering")
}

func TestExecuteTemplate_ForIngressWithIngressNginxProxyHeaders(t *testing.T) {
	t.Parallel()
	upstream := testUpstream
	upstream.Keepalive = "320"
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		cfg := IngressNginxConfig{
			Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com"}},
			Upstreams: []Upstream{upstream},
			Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
		}
		cfg.Servers[0].Locations = []Location{{Path: "/tea", Upstream: upstream, ProxyPass: "http://test", IngressNginxProxyHeaders: true}}
		config := executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config,
			"proxy_set_header Upgrade $http_upgrade;",
			"proxy_set_header Connection $ingress_nginx_connection_upgrade;",
			"proxy_set_header X-Forwarded-For $remote_addr;",
			"proxy_set_header X-Forwarded-Scheme $scheme;",
			"proxy_set_header X-Scheme $scheme;",
			"proxy_set_header X-Original-Forwarded-For $http_x_forwarded_for;",
			"proxy_set_header X-Request-ID $ingress_nginx_req_id;",
			`proxy_set_header Proxy "";`,
		)
		requireNotContains(t, config, `proxy_set_header Connection "";`, "$proxy_add_x_forwarded_for")

		// without keepalive the Connection of a request that is no upgrade is close, as in ingress-nginx
		cfg.Servers[0].Locations[0].Upstream.Keepalive = ""
		config = executeTemplateNamed(t, tmpl, cfg)
		requireContains(t, config, "proxy_set_header Connection $connection_upgrade;")

		cfg.Servers[0].Locations[0].IngressNginxProxyHeaders = false
		config = executeTemplateNamed(t, tmpl, cfg)
		requireNotContains(t, config, "X-Request-ID", "X-Scheme", "proxy_set_header Upgrade")
		requireContains(t, config, "proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;")
	}
}

func TestExecuteTemplate_ForIngressWithHSTSPreload(t *testing.T) {
	t.Parallel()
	for tmpl, variable := range map[string]string{"nginx.ingress.tmpl": "hsts_header_value", "nginx-plus.ingress.tmpl": "hsts_header_val"} {
		server := Server{Name: "cafe.example.com", StatusZone: "cafe.example.com", SSL: true, HSTS: true, HSTSMaxAge: 31536000, HSTSIncludeSubdomains: true}
		cfg := IngressNginxConfig{Servers: []Server{server}, Upstreams: []Upstream{testUpstream}, Ingress: Ingress{Name: "cafe-ingress", Namespace: "default"}}
		requireContains(t, executeTemplateNamed(t, tmpl, cfg), `set $`+variable+` "max-age=31536000; includeSubDomains";`)

		cfg.Servers[0].HSTSPreload = true
		requireContains(t, executeTemplateNamed(t, tmpl, cfg), `set $`+variable+` "max-age=31536000; includeSubDomains; preload";`)

		cfg.Servers[0].HSTSIncludeSubdomains = false
		requireContains(t, executeTemplateNamed(t, tmpl, cfg), `set $`+variable+` "max-age=31536000; preload";`)
	}
}

func executeTemplateNamed(t *testing.T, name string, cfg IngressNginxConfig) string {
	t.Helper()
	tmpl := newNGINXIngressTmpl(t)
	if name == "nginx-plus.ingress.tmpl" {
		tmpl = newNGINXPlusIngressTmpl(t)
	}
	buf := &bytes.Buffer{}
	if err := tmpl.Execute(buf, cfg); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func TestExecuteTemplate_ForIngressWithIngressNginxExternalAuthShared(t *testing.T) {
	t.Parallel()
	auth := &IngressNginxExternalAuth{
		Location:       "/_external-auth-L3RlYQ-Prefix",
		URL:            "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth",
		Host:           "oauth2-proxy.auth.svc.cluster.local",
		SigninLocation: "@0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3",
		SigninURL:      "https://auth.example.com/start",
	}
	for _, tmpl := range []string{"nginx.ingress.tmpl", "nginx-plus.ingress.tmpl"} {
		cfg := IngressNginxConfig{
			Servers:   []Server{{Name: "cafe.example.com", StatusZone: "cafe.example.com"}},
			Upstreams: []Upstream{testUpstream},
			Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
		}
		cfg.Servers[0].Locations = []Location{
			{Path: "/tea/", Upstream: testUpstream, ProxyPass: "http://test", ClientMaxBodySize: "1m", IngressNginxExternalAuth: auth},
			{Path: "= /tea", Upstream: testUpstream, ProxyPass: "http://test", ClientMaxBodySize: "1m", IngressNginxExternalAuth: auth, IngressNginxExternalAuthShared: true},
		}
		config := executeTemplateNamed(t, tmpl, cfg)
		for _, once := range []string{"location = /_external-auth-L3RlYQ-Prefix {", "location @0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3 {"} {
			if n := strings.Count(config, once); n != 1 {
				t.Errorf("%s: %q %d times", tmpl, once, n)
			}
		}
		if n := strings.Count(config, "auth_request /_external-auth-L3RlYQ-Prefix;"); n != 2 {
			t.Errorf("%s: auth_request %d times, want in both locations", tmpl, n)
		}
	}
}

func TestExecuteTemplate_ForIngressWithIngressNginxRedirect(t *testing.T) {
	t.Parallel()
	config := executeIngressTemplateForLocations(t, IngressNginxConfig{},
		Location{Path: "/", Upstream: testUpstream, ProxyPass: "http://test",
			IngressNginxRedirect: &version2.Return{Code: 308, Text: "https://example.org$request_uri"}},
	)
	requireContains(t, config, `return 308 "https://example.org$request_uri";`)
}

func TestExecuteTemplate_ForIngressWithClientCertHTTPSOnly(t *testing.T) {
	t.Parallel()
	server := Server{Name: "mtls.example.com", SSL: true, IngressMTLS: &version2.IngressMTLS{ClientCert: "/etc/nginx/secrets/ca", VerifyClient: "on", VerifyDepth: 1}}
	cfg := IngressNginxConfig{Servers: []Server{server}, Upstreams: []Upstream{testUpstream}, Ingress: Ingress{Name: "mtls", Namespace: "default"}}
	requireNotContains(t, executeTemplateNamed(t, "nginx.ingress.tmpl", cfg), "if ($https != on)")

	cfg.Servers[0].IngressNginxClientCertHTTPSOnly = true
	requireContains(t, executeTemplateNamed(t, "nginx.ingress.tmpl", cfg), "if ($https != on) {\n\t\treturn 503;")
}

// ingress-nginx sends http to https in the rewrite phase of each location, after nginx refused a body over the
// location's limit with 413 and after the location's own returns: the server decides, every location sends.
func TestExecuteTemplate_ForIngressWithIngressNginxHTTPSRedirectInLocations(t *testing.T) {
	t.Parallel()
	for name, tmpl := range map[string]func(*testing.T) *template.Template{"oss": newNGINXIngressTmpl, "plus": newNGINXPlusIngressTmpl} {
		for _, tc := range []struct {
			name      string
			locations []Location
			catchAll  bool
		}{
			{"with a location for /", []Location{
				{Path: "/", Upstream: testUpstream, ProxyPass: "http://test", ClientMaxBodySize: "1m"},
				{Path: "/rw", Upstream: testUpstream, ProxyPass: "http://test", RewriteTarget: "/$2"},
				{Path: "/internal", Upstream: testUpstream, ProxyPass: "http://test", Internal: true},
			}, false},
			{"without one", []Location{{Path: "/tea", Upstream: testUpstream, ProxyPass: "http://test"}}, true},
		} {
			cfg := IngressNginxConfig{
				Servers: []Server{{
					Name: "cafe.example.com", StatusZone: "cafe.example.com", SSL: true, SSLRedirect: true,
					IngressNginxRedirect: true, IngressNginxHTTPSRedirect: true, HTTPRedirectCode: 308, SSLPorts: []int{443},
					Locations: tc.locations,
				}},
				Upstreams: []Upstream{testUpstream},
				Ingress:   Ingress{Name: "cafe-ingress", Namespace: "default"},
			}
			buf := &bytes.Buffer{}
			if err := tmpl(t).Execute(buf, cfg); err != nil {
				t.Fatal(err)
			}
			config := buf.String()
			server, _, _ := strings.Cut(config, "location ")
			requireContains(t, server, "set $ingress_nginx_https_redirect 0;", "set $ingress_nginx_https_redirect 1;")
			requireNotContains(t, server, "return 308")
			sends := strings.Count(config, "if ($ingress_nginx_https_redirect) {\n\t\t\treturn 308 https://$ingress_nginx_redirect_host$ingress_nginx_redirect_uri;")
			want := 2 // "/" and "/rw", not the internal one
			if tc.catchAll {
				want = 2 // "/tea" and the catch-all
				requireContains(t, config, "return 404;")
			}
			if sends != want {
				t.Errorf("%s %s: the redirect in %d locations, want %d:\n%s", name, tc.name, sends, want, config)
			}
			if !tc.catchAll {
				// before rewrite-target's rewrite ... break, which ends the rewrite module's work
				_, rw, _ := strings.Cut(config, "/rw")
				if i, j := strings.Index(rw, "$ingress_nginx_https_redirect"), strings.Index(rw, "rewrite "); i < 0 || j < 0 || i > j {
					t.Errorf("%s: the redirect is not before the rewrite:\n%s", name, rw)
				}
			}
		}
	}
}
