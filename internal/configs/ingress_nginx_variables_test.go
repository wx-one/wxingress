package configs

import (
	"context"
	"slices"
	"strings"
	"testing"

	api_v1 "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs/version2"
)

func TestCheckIngressNginxVariables(t *testing.T) {
	t.Parallel()
	tests := []struct {
		value      string
		bareBraces bool
		valid      bool
	}{
		{"/$2", false, true},
		{"/api/$1", false, true},
		{"https://host/$1", false, true},
		{"/$1$9", false, true},
		{"https://$host/oauth2/start?rd=$escaped_request_uri", false, true},
		{"https://$host/oauth2/auth", false, true},
		{"$scheme://$server_name:$server_port$uri$is_args$args", false, true},
		{"$remote_user$http_authorization", false, true},
		{"$HTTP_X_TENANT", false, true},
		{"$http_x_user$arg_page$cookie_session", false, true},
		{"${request_uri}", false, true},
		{"${host}x", false, true},
		{"$proxy_add_x_forwarded_for", false, true},
		{"$document_root$fastcgi_script_name", false, true},
		{"plain text", false, true},
		{"{json}", true, true},

		{"/$0", false, false},
		{"$", false, false},
		{"/a$", false, false},
		{"$/", false, false},
		{"$foo", false, false},
		{"$http_", false, false},
		{"$pass_access_scheme", false, false},
		{"$best_http_host", false, false},
		{"$ingress_nginx_mirror_uri", false, false},
		{"$upstream_http_x", false, false},
		{"${host", false, false},
		{"${1}", false, false},
		{"${}", false, false},
		{"a{b", false, false},
		{"a}b", false, false},
		{"$host{", false, false},
		{"$foo", true, false},
	}
	for _, tt := range tests {
		err := checkIngressNginxVariables(tt.value, tt.bareBraces)
		if (err == nil) != tt.valid {
			t.Errorf("checkIngressNginxVariables(%q, %v) = %v, want valid %v", tt.value, tt.bareBraces, err, tt.valid)
		}
	}
}

// TestIngressNginxAnnotationsRefuseUnknownVariables checks that the
// annotations that take nginx variables take the captures and the variables
// every server knows and leave out others, which would make nginx -t and the
// reload of every resource fail.
func TestIngressNginxAnnotationsRefuseUnknownVariables(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		taken       func(ConfigParams) bool
		valid       bool
	}{
		{"rewrite-target capture", map[string]string{p + "rewrite-target": "/$2"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget == "/$2" }, true},
		{"rewrite-target path and capture", map[string]string{p + "rewrite-target": "/api/$1"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget == "/api/$1" }, true},
		{"rewrite-target with $host", map[string]string{p + "rewrite-target": "/$host/$1"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget == "/$host/$1" }, true},
		{"rewrite-target unknown", map[string]string{p + "rewrite-target": "/$foo"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget != "" }, false},
		{"rewrite-target $0", map[string]string{p + "rewrite-target": "/$0"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget != "" }, false},
		{"rewrite-target lone $", map[string]string{p + "rewrite-target": "/a$"}, func(c ConfigParams) bool { return c.IngressNginxRewriteTarget != "" }, false},
		{"x-forwarded-prefix", map[string]string{p + "x-forwarded-prefix": "/$1"}, func(c ConfigParams) bool { return c.IngressNginxXForwardedPrefix == "/$1" }, true},
		{"x-forwarded-prefix unknown", map[string]string{p + "x-forwarded-prefix": "/$prefix"}, func(c ConfigParams) bool { return c.IngressNginxXForwardedPrefix != "" }, false},
		{"permanent-redirect", map[string]string{p + "permanent-redirect": "https://example.org$request_uri"}, func(c ConfigParams) bool { return c.IngressNginxRedirect != nil }, true},
		{"permanent-redirect unknown", map[string]string{p + "permanent-redirect": "https://example.org/$target"}, func(c ConfigParams) bool { return c.IngressNginxRedirect != nil }, false},
		{"temporal-redirect unknown", map[string]string{p + "temporal-redirect": "https://example.org/$target"}, func(c ConfigParams) bool { return c.IngressNginxRedirect != nil }, false},
		{"mirror-target", map[string]string{p + "mirror-target": "http://mirror.example/$request_uri"}, func(c ConfigParams) bool {
			return c.IngressNginxMirror != nil && c.IngressNginxMirror.Target == "http://mirror.example/$ingress_nginx_mirror_uri"
		}, true},
		{"mirror-target variable after the host", map[string]string{p + "mirror-target": "http://u$http_x_t"}, func(c ConfigParams) bool { return c.IngressNginxMirror != nil }, false},
		{"mirror-target unix socket", map[string]string{p + "mirror-target": "http://unix:/var/lib/nginx/api.sock:/"}, func(c ConfigParams) bool { return c.IngressNginxMirror != nil }, false},
		{"mirror-target unknown", map[string]string{p + "mirror-target": "http://mirror.example/$target"}, func(c ConfigParams) bool { return c.IngressNginxMirror != nil }, false},
		{"proxy-cookie-path", map[string]string{p + "proxy-cookie-path": "/ /$host/"}, func(c ConfigParams) bool {
			return slices.Contains(c.IngressNginxDirectives, "proxy_cookie_path / /$host/")
		}, true},
		{"proxy-cookie-path regex with anchor and capture", map[string]string{p + "proxy-cookie-path": "~^/(a)$ /$1"}, func(c ConfigParams) bool { return len(c.IngressNginxDirectives) == 1 }, true},
		{"proxy-cookie-path broken regex", map[string]string{p + "proxy-cookie-path": "~^/(a /b"}, func(c ConfigParams) bool { return len(c.IngressNginxDirectives) != 0 }, false},
		{"proxy-cookie-domain unknown", map[string]string{p + "proxy-cookie-domain": "$backend_host example.org"}, func(c ConfigParams) bool { return len(c.IngressNginxDirectives) != 0 }, false},
		{"upstream-hash-by", map[string]string{p + "upstream-hash-by": "$request_uri"}, func(c ConfigParams) bool { return c.LBMethod == "hash $request_uri consistent" }, true},
		{"upstream-hash-by braced", map[string]string{p + "upstream-hash-by": "${remote_addr}x"}, func(c ConfigParams) bool { return c.LBMethod == "hash ${remote_addr}x consistent" }, true},
		{"upstream-hash-by bare brace", map[string]string{p + "upstream-hash-by": "a{b"}, func(c ConfigParams) bool { return strings.HasPrefix(c.LBMethod, "hash") }, false},
		{"upstream-hash-by unknown", map[string]string{p + "upstream-hash-by": "$tenant"}, func(c ConfigParams) bool { return strings.HasPrefix(c.LBMethod, "hash") }, false},
		{"proxy-ssl-name", map[string]string{p + "proxy-ssl-name": "$host"}, func(c ConfigParams) bool {
			return c.IngressNginxProxySSL != nil && c.IngressNginxProxySSL.SSLName == "$host"
		}, true},
		{"proxy-ssl-name unknown", map[string]string{p + "proxy-ssl-name": "$upstream"}, func(c ConfigParams) bool {
			return c.IngressNginxProxySSL == nil || c.IngressNginxProxySSL.SSLName != "$proxy_host"
		}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := parseIngressNginxForTest(t, tt.annotations, false)
			if got := tt.taken(cfg); got != tt.valid {
				t.Errorf("taken %v, want %v; warnings %v", got, tt.valid, warnings)
			}
			if invalid := hasWarningContaining(warnings, "is ignored"); invalid == tt.valid {
				t.Errorf("warnings %v", warnings)
			}
			if cfg.IngressNginxDenied {
				t.Error("the Ingress is denied")
			}
		})
	}
}

func TestIngressNginxExternalAuthVariables(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	// production-realistic values of oauth2-proxy
	cfg, warnings := parseIngressNginxForTest(t, map[string]string{
		p + "auth-url":       "https://$host/oauth2/auth",
		p + "auth-signin":    "https://$host/oauth2/start?rd=$escaped_request_uri",
		p + "auth-cache-key": "$remote_user$http_authorization",
	}, false)
	auth := cfg.IngressNginxExternalAuth
	if auth == nil || cfg.IngressNginxDenied || auth.URL != "https://$host/oauth2/auth" ||
		auth.signinURL != "https://$host/oauth2/start?rd=$escaped_request_uri" || auth.cacheKey != "$remote_user$http_authorization" {
		t.Fatalf("auth %+v, denied %v, warnings %v", auth, cfg.IngressNginxDenied, warnings)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings %v", warnings)
	}

	// an unknown variable in auth-url denies the locations, as any invalid auth-url
	cfg, warnings = parseIngressNginxForTest(t, map[string]string{p + "auth-url": "http://auth.example/$target"}, false)
	if cfg.IngressNginxExternalAuth != nil || !cfg.IngressNginxDenied || !hasWarningContaining(warnings, "$target") {
		t.Errorf("auth-url with an unknown variable: %+v, denied %v, warnings %v", cfg.IngressNginxExternalAuth, cfg.IngressNginxDenied, warnings)
	}

	// auth-signin and auth-cache-key with an unknown variable or a bare brace are ignored
	cfg, warnings = parseIngressNginxForTest(t, map[string]string{
		p + "auth-url":       "http://auth.example/auth",
		p + "auth-signin":    "https://auth.example/start?rd=$target",
		p + "auth-cache-key": "a{b",
	}, false)
	auth = cfg.IngressNginxExternalAuth
	if auth == nil || cfg.IngressNginxDenied || auth.signinURL != "" || auth.cacheKey != "" {
		t.Fatalf("auth %+v, warnings %v", auth, warnings)
	}
	for _, name := range []string{p + "auth-signin is ignored", p + "auth-cache-key is ignored"} {
		if !hasWarningContaining(warnings, name) {
			t.Errorf("warnings %v lack %q", warnings, name)
		}
	}
	cfg, _ = parseIngressNginxForTest(t, map[string]string{p + "auth-url": "http://auth.example/auth", p + "auth-cache-key": "$tenant"}, false)
	if cfg.IngressNginxExternalAuth == nil || cfg.IngressNginxExternalAuth.cacheKey != "" {
		t.Errorf("auth-cache-key with an unknown variable is taken: %+v", cfg.IngressNginxExternalAuth)
	}
}

func TestIngressNginxAuthProxySetHeadersValues(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	ann := map[string]string{IngressNginxAuthURLAnnotation: "http://auth.example/auth", IngressNginxAuthProxySetHeadersAnnotation: "headers"}
	tests := []struct {
		name  string
		data  map[string]string
		valid bool
	}{
		{"variables", map[string]string{"X-User": "$http_x_user", "X-Host": "${host}", "X-Empty": ""}, true},
		{"text with braces", map[string]string{"X-Json": `{"a": 1}`}, true},
		{"CR LF", map[string]string{"X-User": "a\r\nX-Admin: true"}, false},
		{"LF", map[string]string{"X-User": "a\nb"}, false},
		{"control character", map[string]string{"X-User": "a\x00b"}, false},
		{"unknown variable", map[string]string{"X-User": "$tenant"}, false},
		{"lone $", map[string]string{"X-User": "a$"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cms := map[string]*api_v1.ConfigMap{"default/headers": headersConfigMap("default", tt.data)}
			cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, cms, false)
			if tt.valid {
				if cfg.IngressNginxDenied || cfg.IngressNginxExternalAuth == nil || len(cfg.IngressNginxExternalAuth.ProxySetHeaders) != len(tt.data) {
					t.Errorf("auth %+v, denied %v, warnings %v", cfg.IngressNginxExternalAuth, cfg.IngressNginxDenied, warnings)
				}
				return
			}
			if !cfg.IngressNginxDenied || cfg.IngressNginxExternalAuth != nil || !hasWarningContaining(warnings, "has an invalid value") {
				t.Errorf("auth %+v, denied %v, warnings %v", cfg.IngressNginxExternalAuth, cfg.IngressNginxDenied, warnings)
			}
		})
	}
}

func TestIngressNginxFastCGIParamsVariables(t *testing.T) {
	t.Parallel()
	base := NewDefaultConfigParams(context.Background(), false)
	ann := map[string]string{IngressNginxBackendProtocolAnnotation: "FCGI", IngressNginxFastCGIParamsAnnotation: "fcgi"}
	for value, valid := range map[string]bool{
		"$document_root$fastcgi_script_name": true,
		"${document_root}/index.php":         true,
		"$script":                            false,
		"a{b":                                false,
		"${host":                             false,
	} {
		cms := map[string]*api_v1.ConfigMap{"default/fcgi": {Data: map[string]string{"SCRIPT_FILENAME": value}}}
		cfg, warnings := parseIngressNginxMoreForTest(t, ann, *base, cms, false)
		if cfg.IngressNginxDenied == valid {
			t.Errorf("%q: denied %v, warnings %v", value, cfg.IngressNginxDenied, warnings)
		}
		if valid && (cfg.IngressNginxFastCGI == nil || !slices.Equal(cfg.IngressNginxFastCGI.Params, []version2.Header{{Name: "SCRIPT_FILENAME", Value: value}})) {
			t.Errorf("%q: fastcgi %+v", value, cfg.IngressNginxFastCGI)
		}
	}
}

func TestParseIngressNginxCORSRefusesLineBreaks(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	for _, tt := range []struct{ name, value, field string }{
		{"cors-allow-methods", "GET,\r\nX-Injected: 1", "methods"},
		{"cors-allow-methods", "GET,\nPOST", "methods"},
		{"cors-expose-headers", "X-A,\r\nX-Injected", "expose"},
		{"cors-expose-headers", "X-A,\tX-B", "expose"},
	} {
		ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: map[string]string{p + "enable-cors": "true", p + tt.name: tt.value}}}
		a := newIngressNginxAnnotations(ing)
		cors := parseIngressNginxCORS(a)
		if cors.Methods != ingressNginxCORSMethods || cors.ExposeHeaders != "" {
			t.Errorf("%s %q is taken: %+v", tt.name, tt.value, cors)
		}
		if !hasWarningContaining(a.warnings[ing], p+tt.name+" is ignored") {
			t.Errorf("%s %q: warnings %v", tt.name, tt.value, a.warnings[ing])
		}
	}
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: map[string]string{
		p + "enable-cors": "true", p + "cors-allow-methods": "GET, POST,PUT", p + "cors-expose-headers": "X-A, X-B,*",
	}}}
	cors := parseIngressNginxCORS(newIngressNginxAnnotations(ing))
	if cors.Methods != "GET, POST,PUT" || cors.ExposeHeaders != "X-A, X-B,*" {
		t.Errorf("valid values are not taken: %+v", cors)
	}
}

// TestWithIngressNginxAliasesValidatesLikeNginxOrg checks that the aliases
// get the checks the controller makes of the nginx.org annotations they
// become, which it does not make of what an alias adds.
func TestWithIngressNginxAliasesValidatesLikeNginxOrg(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]string
		warning     string
	}{
		{"upstream-vhost $host", map[string]string{p + "upstream-vhost": "$host"}, map[string]string{UpstreamVhostAnnotation: "$host"}, ""},
		{"upstream-vhost unknown variable", map[string]string{p + "upstream-vhost": "$tenant.internal"}, map[string]string{UpstreamVhostAnnotation: ""}, "$tenant"},
		{"app-root unknown variable", map[string]string{p + "app-root": "/$app"}, map[string]string{"nginx.org/app-root": ""}, "$app"},
		{"proxy-redirect", map[string]string{p + "proxy-redirect-from": "http://backend/", p + "proxy-redirect-to": "https://$host/"},
			map[string]string{ProxyRedirectFromAnnotation: "http://backend/", ProxyRedirectToAnnotation: "https://$host/"}, ""},
		{"proxy-redirect regex", map[string]string{p + "proxy-redirect-from": "~^http://backend/(.*)$", p + "proxy-redirect-to": "/$1"},
			map[string]string{ProxyRedirectFromAnnotation: "~^http://backend/(.*)$", ProxyRedirectToAnnotation: "/$1"}, ""},
		{"proxy-redirect-from off", map[string]string{p + "proxy-redirect-from": "off"}, map[string]string{ProxyRedirectFromAnnotation: "off"}, ""},
		{"proxy-redirect-from #", map[string]string{p + "proxy-redirect-from": "#", p + "proxy-redirect-to": "/"},
			map[string]string{ProxyRedirectFromAnnotation: "", ProxyRedirectToAnnotation: ""}, "is ignored"},
		{"proxy-redirect-from comment", map[string]string{p + "proxy-redirect-from": "http://a/#x", p + "proxy-redirect-to": "/"},
			map[string]string{ProxyRedirectFromAnnotation: ""}, "is ignored"},
		{"proxy-redirect-to backtick", map[string]string{p + "proxy-redirect-from": "http://a/", p + "proxy-redirect-to": "/`x"},
			map[string]string{ProxyRedirectFromAnnotation: "", ProxyRedirectToAnnotation: ""}, "is ignored"},
		{"proxy-redirect-to unknown variable", map[string]string{p + "proxy-redirect-from": "http://a/", p + "proxy-redirect-to": "/$x"},
			map[string]string{ProxyRedirectToAnnotation: ""}, "$x"},
		{"proxy-redirect-from broken regex", map[string]string{p + "proxy-redirect-from": "~^http://(a", p + "proxy-redirect-to": "/"},
			map[string]string{ProxyRedirectFromAnnotation: ""}, "regular expression"},
		{"proxy-redirect-from balancing group", map[string]string{p + "proxy-redirect-from": "~(?<a>x)(?<-a>y)", p + "proxy-redirect-to": "/"},
			map[string]string{ProxyRedirectFromAnnotation: ""}, "balancing group"},
		{"proxy-redirect-from without to", map[string]string{p + "proxy-redirect-from": "http://a/"}, map[string]string{ProxyRedirectFromAnnotation: ""}, "needs"},
		{"proxy-redirect-to without from", map[string]string{p + "proxy-redirect-to": "/"}, map[string]string{ProxyRedirectToAnnotation: ""}, "needs"},
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

func TestIngressNginxServerAliasWildcards(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "ing", Annotations: map[string]string{
		p + "server-alias": "*.com, *, *.example.com, B.example.org, *.*.example.com, example.*",
	}}}
	a := newIngressNginxAnnotations(ing)
	cfg := &ConfigParams{}
	parseIngressNginxServerAnnotations(a, cfg)
	if !slices.Equal(cfg.IngressNginxServerAliases, []string{"*.example.com", "B.example.org"}) {
		t.Errorf("aliases %q", cfg.IngressNginxServerAliases)
	}
	if len(a.warnings[ing]) != 4 {
		t.Errorf("warnings %q", a.warnings[ing])
	}

	if got := IngressNginxServerAliases(ing); !slices.Equal(got, []string{"*.example.com", "b.example.org"}) {
		t.Errorf("IngressNginxServerAliases %q", got)
	}
	if got := IngressNginxServerAliases(&networking.Ingress{}); got != nil {
		t.Errorf("IngressNginxServerAliases without the annotation %q", got)
	}

	if same := WithoutIngressNginxServerAliases(ing, func(string) bool { return false }); same != ing {
		t.Error("an Ingress that drops no alias is copied")
	}
	dropped := WithoutIngressNginxServerAliases(ing, func(alias string) bool { return alias == "b.example.org" })
	if dropped == ing || dropped.Annotations[p+"server-alias"] != "*.example.com" {
		t.Errorf("server-alias %q", dropped.Annotations[p+"server-alias"])
	}
	if ing.Annotations[p+"server-alias"] != "*.com, *, *.example.com, B.example.org, *.*.example.com, example.*" {
		t.Error("the Ingress given is changed")
	}
	none := WithoutIngressNginxServerAliases(ing, func(string) bool { return true })
	if _, exists := none.Annotations[p+"server-alias"]; exists {
		t.Errorf("server-alias %q", none.Annotations[p+"server-alias"])
	}
}

func TestCheckPCRERegex(t *testing.T) {
	t.Parallel()
	for pattern, valid := range map[string]bool{
		`^/api/(.*)`:              true,
		`/(?<name>[a-z]+)/(\d+)`:  true,
		`/(?'name'[a-z]+)`:        true,
		`/(?<name>a)(?(name)b|c)`: true,
		`/(a)(?(1)b|c)`:           true,
		`/(?(?=a)ab|cd)`:          true,
		`/(?<=ab)c`:               true,
		`/(?<!a{2})c`:             true,
		`/(?<=a|bc)d`:             true,
		`/(?i)foo`:                true,
		`/[(?<-a>)]`:              true,
		`/\(?<-a>y\)`:             true,
		`/\p{L}+`:                 true,
		`/(?<a>x)(?<-a>y)`:        false,
		`/(?<a>x)(?<b-a>y)`:       false,
		`/(?'a'x)(?'-a'y)`:        false,
		`/(?<1>x)`:                false,
		`/(?(abc)x|y)`:            false,
		`/(?<=a+)b`:               false,
		`/(?<=a*)b`:               false,
		`/(?<!a?)b`:               false,
		`/(?<=a{1,3})b`:           false,
		`/(?<=(ab)+)c`:            false,
		`/\p{IsGreek}`:            false,
		`/(unclosed`:              false,
	} {
		if err := CheckPCRERegex(pattern); (err == nil) != valid {
			t.Errorf("CheckPCRERegex(%q) = %v, want valid %v", pattern, err, valid)
		}
	}
}
