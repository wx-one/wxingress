package configs

import (
	"testing"

	networking "k8s.io/api/networking/v1"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/nginx"
)

func upstreamOf(cfg version1.IngressNginxConfig, name string) *version1.Upstream {
	for i := range cfg.Upstreams {
		if cfg.Upstreams[i].Name == name {
			return &cfg.Upstreams[i]
		}
	}
	return nil
}

func TestGenerateNginxCfgWithIngressNginxAffinity(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxAffinityAnnotation] = "cookie"
	ingEx.Ingress.Annotations[IngressNginxSessionCookieMaxAgeAnnotation] = "172800"
	ingEx.Ingress.Annotations[IngressNginxSessionCookieExpiresAnnotation] = "3600"
	ingEx.Ingress.Annotations[IngressNginxSessionCookieSameSiteAnnotation] = "Lax"

	cfg, warnings := generateCanaryCfg(t, &ingEx, false)
	if len(warnings[ingEx.Ingress]) != 0 {
		t.Errorf("warnings: %v", warnings)
	}

	// the cafe Ingress has TLS and redirects to HTTPS by default, so the cookie is Secure as ingress-nginx makes it there
	tea := upstreamOf(cfg, "default_cafe-ingress_cafe.example.com_tea-svc_80")
	if want := "INGRESSCOOKIE expires=172800s httponly secure samesite=lax path=/tea"; tea == nil || tea.StickyCookie != want {
		t.Errorf("tea is sticky with %q, want %q", tea.StickyCookie, want)
	}
	if tea.LBMethod != "random" {
		t.Errorf("new sessions go by %q, want random", tea.LBMethod)
	}
	coffee := upstreamOf(cfg, "default_cafe-ingress_cafe.example.com_coffee-svc_80")
	if coffee == nil || coffee.StickyCookie != "INGRESSCOOKIE expires=172800s httponly secure samesite=lax path=/coffee" {
		t.Errorf("coffee is sticky with %q", coffee.StickyCookie)
	}
}

func TestIngressNginxStickyCookie(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		paths       []string
		onlyHTTPS   bool
		want        string
		wantWarning string
	}{
		{
			name:        "defaults",
			annotations: map[string]string{IngressNginxAffinityAnnotation: "cookie"},
			paths:       []string{"/"},
			want:        "INGRESSCOOKIE httponly path=/",
		},
		{
			name: "everything",
			annotations: map[string]string{
				IngressNginxAffinityAnnotation:                      "cookie",
				IngressNginxSessionCookieNameAnnotation:             "route",
				IngressNginxSessionCookiePathAnnotation:             "/app",
				IngressNginxSessionCookieDomainAnnotation:           ".example.com",
				IngressNginxSessionCookieExpiresAnnotation:          "60",
				IngressNginxSessionCookieSecureAnnotation:           "true",
				IngressNginxSessionCookieSameSiteAnnotation:         "None",
				IngressNginxSessionCookieConditionalSameSiteNoneAnn: "true",
			},
			paths: []string{"/app/a", "/app/b"},
			want:  "route expires=60s domain=.example.com httponly secure samesite=$ingress_nginx_samesite_none path=/app",
		},
		{
			name:        "the common prefix of the paths",
			annotations: map[string]string{IngressNginxAffinityAnnotation: "cookie"},
			paths:       []string{"/api/v1/users", "/api/v2"},
			onlyHTTPS:   true,
			want:        "INGRESSCOOKIE httponly secure path=/api",
		},
		{
			name:        "a lowercase SameSite, which ingress-nginx drops",
			annotations: map[string]string{IngressNginxAffinityAnnotation: "cookie", IngressNginxSessionCookieSameSiteAnnotation: "lax"},
			paths:       []string{"/"},
			want:        "INGRESSCOOKIE httponly path=/",
			wantWarning: IngressNginxSessionCookieSameSiteAnnotation,
		},
		{
			name:        "a negative max-age",
			annotations: map[string]string{IngressNginxAffinityAnnotation: "cookie", IngressNginxSessionCookieMaxAgeAnnotation: "-1"},
			paths:       []string{"/"},
			want:        "INGRESSCOOKIE httponly path=/",
			wantWarning: IngressNginxSessionCookieMaxAgeAnnotation,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			a := newIngressNginxAnnotations(ingressNginxTestIngress(test.annotations).Ingress)
			affinity := parseIngressNginxAffinity(a)
			if affinity == nil {
				t.Fatal("no affinity")
			}
			if got := affinity.stickyCookie(test.paths, test.onlyHTTPS); got != test.want {
				t.Errorf("stickyCookie() = %q, want %q", got, test.want)
			}
			if test.wantWarning != "" && !hasWarningContaining(a.warnings[a.ing], test.wantWarning) {
				t.Errorf("warnings %v lack %q", a.warnings, test.wantWarning)
			}
		})
	}
}

func TestParseIngressNginxLoadBalancing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		want        string
		wantWarning string
	}{
		{name: "upstream-hash-by", annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "$request_uri"}, want: "hash $request_uri consistent"},
		{name: "ewma", annotations: map[string]string{IngressNginxLoadBalanceAnnotation: "ewma"}, want: "least_time last_byte"},
		{name: "round_robin", annotations: map[string]string{IngressNginxLoadBalanceAnnotation: "round_robin"}, want: ""},
		{
			name:        "affinity wins over upstream-hash-by",
			annotations: map[string]string{IngressNginxAffinityAnnotation: "cookie", IngressNginxUpstreamHashByAnnotation: "$request_uri"},
			want:        "random",
		},
		{
			name:        "upstream-hash-by wins over load-balance",
			annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "${remote_addr}x", IngressNginxLoadBalanceAnnotation: "ewma"},
			want:        "hash ${remote_addr}x consistent",
		},
		{
			name:        "nginx.org/lb-method wins",
			annotations: map[string]string{IngressNginxLoadBalanceAnnotation: "ewma", "nginx.org/lb-method": "least_conn"},
			want:        "least_conn",
			wantWarning: "nginx.org/lb-method is set",
		},
		{
			name:        "a subset is one endpoint for nginx's hash",
			annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "$arg_id", IngressNginxUpstreamHashBySubsetAnnotation: "true"},
			want:        "hash $arg_id consistent",
		},
		{
			name:        "an invalid hash-by",
			annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "$request_uri;"},
			want:        "random two least_conn",
			wantWarning: IngressNginxUpstreamHashByAnnotation + " is ignored",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := parseIngressNginxForTest(t, test.annotations, false)
			if cfg.LBMethod != test.want {
				t.Errorf("LBMethod = %q, want %q", cfg.LBMethod, test.want)
			}
			if test.wantWarning != "" && !hasWarningContaining(warnings, test.wantWarning) {
				t.Errorf("warnings %v lack %q", warnings, test.wantWarning)
			}
		})
	}
}

func TestGenerateNginxCfgWithIngressNginxAffinityAndCanary(t *testing.T) {
	t.Parallel()
	const primary = "default_cafe-ingress_cafe.example.com_tea-svc_80"
	const canary = "default_cafe-canary_cafe.example.com_tea-canary-svc_80"

	for _, behavior := range []string{"sticky", "legacy"} {
		t.Run(behavior, func(t *testing.T) {
			t.Parallel()
			ingEx := createCafeIngressEx()
			ingEx.Ingress.Annotations[IngressNginxAffinityAnnotation] = "cookie"
			ingEx.Ingress.Annotations[IngressNginxAffinityCanaryBehaviorAnnotation] = behavior
			ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})}

			cfg, _ := generateCanaryCfg(t, &ingEx, true)

			// the balancers' cookie names the side a session is pinned to, so the rule needs none
			want := nginx.Canary{Canary: canary, Weight: 10, WeightTotal: 100}
			if got := cfg.Canaries[primary]; got != want {
				t.Errorf("canary rule %+v, want %+v", got, want)
			}
			canaryUpstream := upstreamOf(cfg, canary)
			if canaryUpstream == nil {
				t.Fatal("no canary upstream")
			}
			if canaryUpstream.StickyCookie != "" || !canaryUpstream.MetaBalancer {
				t.Errorf("the canary upstream is %+v, want meta_balancer and no sticky", canaryUpstream)
			}
			if got := cfg.Balancers[primary]; got.Algorithm != "sticky_balanced" || got.Backend != "default-tea-svc-80" {
				t.Errorf("the primary's balancer is %+v", got)
			}
			// as in ingress-nginx, the canary is sticky like its primary unless the behavior is legacy
			got, sticky := cfg.Balancers[canary]
			if sticky != (behavior == "sticky") {
				t.Errorf("the canary's balancer is %+v", got)
			}
			if sticky && (got.Backend != "default-tea-canary-svc-80" || got.CookieName != "INGRESSCOOKIE") {
				t.Errorf("the canary's balancer is %+v", got)
			}
		})
	}
}

func TestCommonPathPrefix(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"/":      {"/a", "/b"},
		"/tea":   {"/tea"},
		"/a":     {"/a/b", "/a/c/d"},
		"/a/b":   {"/a/b", "/a/b"},
		"/x":     {"/x/y", "/x"},
		"/empty": {"/empty/", "/empty/a"},
	}
	for want, paths := range tests {
		if got := commonPathPrefix(paths); got != want {
			t.Errorf("commonPathPrefix(%v) = %q, want %q", paths, got, want)
		}
	}
}

func TestIngressNginxBalancers(t *testing.T) {
	t.Parallel()
	const tea = "default_cafe-ingress_cafe.example.com_tea-svc_80"
	const coffee = "default_cafe-ingress_cafe.example.com_coffee-svc_80"

	tests := []struct {
		name        string
		annotations map[string]string
		want        nginx.Balancer
		none        bool
	}{
		{name: "none", annotations: map[string]string{}, none: true},
		{
			name:        "round_robin",
			annotations: map[string]string{IngressNginxLoadBalanceAnnotation: "round_robin"},
			want:        nginx.Balancer{Algorithm: "round_robin", Backend: "default-tea-svc-80"},
		},
		{
			name:        "ewma",
			annotations: map[string]string{IngressNginxLoadBalanceAnnotation: "ewma"},
			want:        nginx.Balancer{Algorithm: "ewma", Backend: "default-tea-svc-80"},
		},
		{
			name:        "upstream-hash-by",
			annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "$request_uri", IngressNginxLoadBalanceAnnotation: "ewma"},
			want:        nginx.Balancer{Algorithm: "chash", HashBy: "$request_uri", Backend: "default-tea-svc-80"},
		},
		{
			name: "a subset",
			annotations: map[string]string{
				IngressNginxUpstreamHashByAnnotation: "$host$uri", IngressNginxUpstreamHashBySubsetAnnotation: "true",
				IngressNginxUpstreamHashBySubsetSizeAnnotation: "4",
			},
			want: nginx.Balancer{Algorithm: "chashsubset", HashBy: "$host$uri", SubsetSize: 4, Backend: "default-tea-svc-80"},
		},
		{
			name:        "a subset of the default size",
			annotations: map[string]string{IngressNginxUpstreamHashByAnnotation: "$uri", IngressNginxUpstreamHashBySubsetAnnotation: "true"},
			want:        nginx.Balancer{Algorithm: "chashsubset", HashBy: "$uri", SubsetSize: 3, Backend: "default-tea-svc-80"},
		},
		{
			name: "affinity balanced",
			annotations: map[string]string{
				IngressNginxAffinityAnnotation: "cookie", IngressNginxUpstreamHashByAnnotation: "$uri",
				IngressNginxSessionCookieNameAnnotation: "route", IngressNginxSessionCookieMaxAgeAnnotation: "60",
				IngressNginxSessionCookieExpiresAnnotation: "120", IngressNginxSessionCookieSameSiteAnnotation: "None",
				IngressNginxSessionCookieConditionalSameSiteNoneAnn: "true", IngressNginxSessionCookieDomainAnnotation: "example.com",
			},
			want: nginx.Balancer{
				Algorithm: "sticky_balanced", Backend: "default-tea-svc-80", CookieName: "route", CookiePath: "/tea",
				CookieDomain: "example.com", CookieExpires: "120", CookieMaxAge: "60", CookieSameSite: "None",
				CookieConditionalSameSiteNone: true,
			},
		},
		{
			name: "affinity persistent",
			annotations: map[string]string{
				IngressNginxAffinityAnnotation: "cookie", IngressNginxAffinityModeAnnotation: "persistent",
				IngressNginxSessionCookieChangeOnFailureAnnotation: "true", IngressNginxSessionCookieSecureAnnotation: "true",
				IngressNginxSessionCookiePathAnnotation: "/",
			},
			want: nginx.Balancer{
				Algorithm: "sticky_persistent", Backend: "default-tea-svc-80", CookieName: "INGRESSCOOKIE", CookiePath: "/",
				CookieSecure: true, ChangeOnFailure: true,
			},
		},
	}

	plain := createCafeIngressEx()
	plainCfg, _ := generateCanaryCfg(t, &plain, true)

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ingEx := createCafeIngressEx()
			for k, v := range test.annotations {
				ingEx.Ingress.Annotations[k] = v
			}
			cfg, _ := generateCanaryCfg(t, &ingEx, true)

			// with the module the upstreams do not change, so a change of rule reloads nothing
			for _, name := range []string{tea, coffee} {
				got, want := upstreamOf(cfg, name), upstreamOf(plainCfg, name)
				if got == nil || want == nil || !got.MetaBalancer || got.LBMethod != want.LBMethod || got.StickyCookie != "" {
					t.Errorf("upstream %s is %+v, want %+v", name, got, want)
				}
			}

			got, exists := cfg.Balancers[tea]
			if exists == test.none {
				t.Fatalf("balancers %+v", cfg.Balancers)
			}
			if !test.none && got != test.want {
				t.Errorf("balancer %+v, want %+v", got, test.want)
			}
			if !test.none {
				if other := cfg.Balancers[coffee]; other.Backend != "default-coffee-svc-80" || other.Algorithm != test.want.Algorithm {
					t.Errorf("coffee's balancer %+v", other)
				}
			}
		})
	}
}

func TestIngressNginxBalancersWithoutTheModule(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxLoadBalanceAnnotation] = "ewma"

	cfg, _ := generateCanaryCfg(t, &ingEx, false)

	tea := upstreamOf(cfg, "default_cafe-ingress_cafe.example.com_tea-svc_80")
	if len(cfg.Balancers) != 0 || tea == nil || tea.MetaBalancer || tea.LBMethod != "least_time last_byte" {
		t.Errorf("without -dynamic-upstreams nginx balances: %+v, %+v", cfg.Balancers, tea)
	}
}

func TestIngressNginxBackendName(t *testing.T) {
	t.Parallel()
	number := networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: "tea", Port: networking.ServiceBackendPort{Number: 8080}}}
	named := networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: "tea", Port: networking.ServiceBackendPort{Name: "http"}}}
	if got := ingressNginxBackendName("ns", &number); got != "ns-tea-8080" {
		t.Errorf("by number %q", got)
	}
	if got := ingressNginxBackendName("ns", &named); got != "ns-tea-http" {
		t.Errorf("by name %q", got)
	}
}
