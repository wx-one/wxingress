package configs

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/wx-one/wxingress/internal/configs/version1"
	networking "k8s.io/api/networking/v1"
)

func TestGenerateNginxCfgWithIngressNginxExternalAuth(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxAuthURLAnnotation] = "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth"
	ingEx.Ingress.Annotations[IngressNginxAuthSigninAnnotation] = "https://auth.example.com/oauth2/start"
	ingEx.Ingress.Annotations[IngressNginxAuthResponseHeadersAnnotation] = "X-Auth-Request-User, X-Auth-Request-Email"
	ingEx.Ingress.Annotations[IngressNginxAuthMethodAnnotation] = "GET"
	ingEx.Ingress.Annotations[IngressNginxAuthAlwaysSetCookieAnnotation] = "true"
	prefix := networking.PathTypePrefix
	ingEx.Ingress.Spec.Rules[0].HTTP.Paths[1].PathType = &prefix

	cfg, warnings := generateNginxCfg(NginxCfgParams{
		ingEx:         &ingEx,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{SystemResolvers: []string{"10.96.0.10"}},
	})
	if len(warnings[ingEx.Ingress]) != 0 {
		t.Errorf("warnings: %v", warnings)
	}

	// the Prefix path /tea is /tea/ and = /tea, which shares the internal locations of the auth
	tea := findLocation(cfg, "/tea/")
	if tea == nil || tea.IngressNginxExternalAuth == nil || tea.IngressNginxExternalAuthShared {
		t.Fatalf("/tea/ is %+v", tea)
	}
	if exact := findLocation(cfg, "= /tea"); exact == nil || exact.IngressNginxExternalAuth != tea.IngressNginxExternalAuth || !exact.IngressNginxExternalAuthShared {
		t.Errorf("= /tea is %+v", exact)
	}
	auth := tea.IngressNginxExternalAuth
	want := version1.IngressNginxExternalAuth{
		Location:        "/_external-auth-L3RlYQ-Prefix",
		URL:             "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth",
		Host:            "oauth2-proxy.auth.svc.cluster.local",
		Method:          "GET",
		AlwaysSetCookie: true,
		SigninURL:       "https://auth.example.com/oauth2/start?rd=$pass_access_scheme://$http_host$escaped_request_uri",
		CacheDurations:  []string{"200 202 401 5m"},
		ResponseHeaders: []version1.IngressNginxAuthHeader{
			{Name: "X-Auth-Request-User", Variable: "$upstream_http_x_auth_request_user"},
			{Name: "X-Auth-Request-Email", Variable: "$upstream_http_x_auth_request_email"},
		},
	}
	if auth.Location != want.Location || auth.URL != want.URL || auth.Host != want.Host || auth.Method != want.Method ||
		auth.AlwaysSetCookie != want.AlwaysSetCookie || auth.SigninURL != want.SigninURL ||
		!slices.Equal(auth.ResponseHeaders, want.ResponseHeaders) || !slices.Equal(auth.CacheDurations, want.CacheDurations) {
		t.Errorf("external auth\n%+v, want\n%+v", *auth, want)
	}
	if len(auth.SigninLocation) != 41 || auth.SigninLocation[0] != '@' {
		t.Errorf("sign-in location %q", auth.SigninLocation)
	}
	if tea.AddHeaderInherit != "merge" {
		t.Errorf("add_header_inherit %q, want merge", tea.AddHeaderInherit)
	}
	if coffee := findLocation(cfg, "/coffee"); coffee.IngressNginxExternalAuth == nil || coffee.IngressNginxExternalAuth.Location != "/_external-auth-L2NvZmZlZQ-default" {
		t.Errorf("/coffee has %+v", coffee.IngressNginxExternalAuth)
	}
}

func TestIngressNginxSigninLocationIsThatOfIngressNginx(t *testing.T) {
	t.Parallel()
	auth := &ingressNginxExternalAuth{signinURL: "https://auth.example.com/start", signinRedirectParam: "rd"}
	// sha1("/tea" + "https://auth.example.com/start")
	if got := auth.forLocation("cafe.example.com", "/tea", nil).SigninLocation; got != "@0ea01ebf668d71e25878dce3a8859a1d4ae1c0c3" {
		t.Errorf("sign-in location %q", got)
	}
}

func TestIngressNginxSigninURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		url, param, want string
	}{
		{"https://a.example.com/start", "rd", "https://a.example.com/start?rd=$pass_access_scheme://$http_host$escaped_request_uri"},
		{"https://a.example.com/start?x=1", "rd", "https://a.example.com/start?x=1&rd=$pass_access_scheme://$http_host$escaped_request_uri"},
		{"https://$host/oauth2/start?rd=$escaped_request_uri", "rd", "https://$host/oauth2/start?rd=$escaped_request_uri"},
		{"https://a.example.com/start", "redirect", "https://a.example.com/start?redirect=$pass_access_scheme://$http_host$escaped_request_uri"},
	}
	for _, test := range tests {
		if got := ingressNginxSigninURL(test.url, test.param); got != test.want {
			t.Errorf("ingressNginxSigninURL(%q, %q) = %q, want %q", test.url, test.param, got, test.want)
		}
	}
}

func TestParseIngressNginxExternalAuth(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		wantAuth    bool
		wantDenied  bool
		wantWarning string
	}{
		{name: "no auth-url"},
		{
			name:        "an auth-url without scheme denies",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "oauth2-proxy/auth"},
			wantDenied:  true, wantWarning: "has no scheme",
		},
		{
			name:        "an invalid auth-method denies",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthMethodAnnotation: "get"},
			wantDenied:  true, wantWarning: IngressNginxAuthMethodAnnotation,
		},
		{
			name:        "an invalid response header denies",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthResponseHeadersAnnotation: "X-User;"},
			wantDenied:  true, wantWarning: IngressNginxAuthResponseHeadersAnnotation,
		},
		{
			name:        "an invalid cache duration denies",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthCacheKeyAnnotation: "$remote_user", IngressNginxAuthCacheDurationAnnotation: "200 abc"},
			wantDenied:  true, wantWarning: IngressNginxAuthCacheDurationAnnotation,
		},
		{
			name:        "an invalid auth-signin is ignored",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthSigninAnnotation: "https://a/%20"},
			wantAuth:    true, wantWarning: IngressNginxAuthSigninAnnotation + " is ignored",
		},
		{
			name:        "auth-snippet without snippets",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthSnippetAnnotation: "proxy_set_header X-A b;"},
			wantAuth:    true, wantWarning: "snippets are not enabled",
		},
		{
			name:        "auth-proxy-set-headers of a ConfigMap that does not exist denies",
			annotations: map[string]string{IngressNginxAuthURLAnnotation: "http://auth/", IngressNginxAuthProxySetHeadersAnnotation: "auth-headers"},
			wantDenied:  true, wantWarning: "does not exist",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := parseIngressNginxForTest(t, test.annotations, false)
			if (cfg.IngressNginxExternalAuth != nil) != test.wantAuth || cfg.IngressNginxDenied != test.wantDenied {
				t.Errorf("auth %+v, denied %v", cfg.IngressNginxExternalAuth, cfg.IngressNginxDenied)
			}
			if test.wantWarning != "" && !hasWarningContaining(warnings, test.wantWarning) {
				t.Errorf("warnings %v lack %q", warnings, test.wantWarning)
			}
		})
	}
}

func TestReadSystemResolvers(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	content := "search default.svc.cluster.local svc.cluster.local\nnameserver 10.96.0.10\nnameserver fd00::a\nnameserver bad\noptions ndots:5\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, want := ReadSystemResolvers(path), []string{"10.96.0.10", "[fd00::a]"}; !slices.Equal(got, want) {
		t.Errorf("ReadSystemResolvers() = %v, want %v", got, want)
	}
	if got := ReadSystemResolvers(filepath.Join(t.TempDir(), "missing")); got != nil {
		t.Errorf("ReadSystemResolvers() of a missing file = %v", got)
	}
}

func TestGenerateNginxCfgWithIngressNginxExternalAuthResolver(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxAuthURLAnnotation] = "http://oauth2-proxy.auth.svc.cluster.local/oauth2/auth"

	cfg, _ := generateNginxCfg(NginxCfgParams{
		ingEx:         &ingEx,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{SystemResolvers: []string{"10.96.0.10"}},
	})
	if tea := findLocation(cfg, "/tea"); tea == nil || tea.IngressNginxExternalAuth == nil || tea.IngressNginxExternalAuth.Resolver != "10.96.0.10 valid=30s" {
		t.Errorf("/tea is %+v", tea)
	}

	cfg, _ = generateNginxCfg(NginxCfgParams{
		ingEx:                &ingEx,
		BaseCfgParams:        NewDefaultConfigParams(context.Background(), false),
		staticParams:         &StaticConfigParams{SystemResolvers: []string{"10.96.0.10"}},
		isResolverConfigured: true,
	})
	if tea := findLocation(cfg, "/tea"); tea == nil || tea.IngressNginxExternalAuth == nil || tea.IngressNginxExternalAuth.Resolver != "" {
		t.Errorf("/tea with the resolver of the ConfigMap is %+v", tea)
	}
}

func TestParseIngressNginxExternalAuthCache(t *testing.T) {
	t.Parallel()
	ingEx := ingressNginxTestIngress(map[string]string{
		IngressNginxAuthURLAnnotation:           "http://$host/auth",
		IngressNginxAuthCacheKeyAnnotation:      "$remote_user$http_authorization",
		IngressNginxAuthCacheDurationAnnotation: "200 202 10m, 401 1m",
		IngressNginxAuthSnippetAnnotation:       "proxy_set_header X-A b;",
	})
	cfg := NewDefaultConfigParams(context.Background(), false)
	parseIngressNginxAnnotations(ingEx, cfg, false, true)
	auth := cfg.IngressNginxExternalAuth
	if auth == nil || auth.Snippet != "proxy_set_header X-A b;" {
		t.Fatalf("auth %+v", auth)
	}
	loc := auth.forLocation("cafe.example.com", "/", nil)
	if loc.CacheZone != "auth_cache_default_app" || loc.CacheKey != "cafe.example.com/_external-auth-Lw-default$remote_user$http_authorization" ||
		!slices.Equal(loc.CacheDurations, []string{"200 202 10m", "401 1m"}) {
		t.Errorf("cache %q %q %v", loc.CacheZone, loc.CacheKey, loc.CacheDurations)
	}
	if caches := ingressNginxAuthCachesOf(auth); len(caches) != 1 || caches[0].Path != "/var/cache/nginx/auth_cache_default_app" {
		t.Errorf("caches %+v", caches)
	}
}
