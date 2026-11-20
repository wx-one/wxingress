package configs

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ingressNginxTestIngress(annotations map[string]string) *IngressEx {
	return &IngressEx{Ingress: &networking.Ingress{
		ObjectMeta: meta_v1.ObjectMeta{Name: "app", Namespace: "default", Annotations: annotations},
	}}
}

// parseIngressNginxForTest parses the annotations over the default config params.
func parseIngressNginxForTest(t *testing.T, annotations map[string]string, autoadjust bool) (ConfigParams, []string) {
	t.Helper()
	ingEx := ingressNginxTestIngress(annotations)
	base := NewDefaultConfigParams(context.Background(), false)
	cfgParams := parseAnnotations(ingEx, base, false, false, false, autoadjust)
	warnings := parseIngressNginxAnnotations(ingEx, &cfgParams, autoadjust, false)
	return cfgParams, warnings[ingEx.Ingress]
}

func hasWarningContaining(warnings []string, part string) bool {
	return slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, part) })
}

func TestParseIngressNginxProxyAnnotations(t *testing.T) {
	t.Parallel()
	cfg, warnings := parseIngressNginxForTest(t, map[string]string{
		IngressNginxProxyConnectTimeoutAnnotation:      "5",
		IngressNginxProxyReadTimeoutAnnotation:         "3600",
		IngressNginxProxySendTimeoutAnnotation:         " 120 ",
		IngressNginxProxyBodySizeAnnotation:            "8m",
		IngressNginxClientBodyBufferSizeAnnotation:     "1M",
		IngressNginxProxyBufferingAnnotation:           "off",
		IngressNginxProxyRequestBufferingAnnotation:    "off",
		IngressNginxProxyBufferSizeAnnotation:          "16k",
		IngressNginxProxyBuffersNumberAnnotation:       "8",
		IngressNginxProxyMaxTempFileSizeAnnotation:     "0",
		IngressNginxProxyHTTPVersionAnnotation:         "1.0",
		IngressNginxProxyNextUpstreamAnnotation:        "non_idempotent error http_502",
		IngressNginxProxyNextUpstreamTimeoutAnnotation: "10",
		IngressNginxProxyNextUpstreamTriesAnnotation:   "0",
	}, false)

	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	checks := map[string][2]string{
		"ProxyConnectTimeout":      {cfg.ProxyConnectTimeout, "5s"},
		"ProxyReadTimeout":         {cfg.ProxyReadTimeout, "3600s"},
		"ProxySendTimeout":         {cfg.ProxySendTimeout, "120s"},
		"ClientMaxBodySize":        {cfg.ClientMaxBodySize, "8m"},
		"ClientBodyBufferSize":     {cfg.ClientBodyBufferSize, "1M"},
		"ProxyRequestBuffering":    {cfg.ProxyRequestBuffering, "off"},
		"ProxyBufferSize":          {cfg.ProxyBufferSize, "16k"},
		"ProxyBuffers":             {cfg.ProxyBuffers, "8 16k"},
		"ProxyMaxTempFileSize":     {cfg.ProxyMaxTempFileSize, "0"},
		"ProxyHTTPVersion":         {cfg.ProxyHTTPVersion, "1.0"},
		"ProxyNextUpstream":        {cfg.ProxyNextUpstream, "error http_502 non_idempotent"},
		"ProxyNextUpstreamTimeout": {cfg.ProxyNextUpstreamTimeout, "10"},
	}
	for name, c := range checks {
		if c[0] != c[1] {
			t.Errorf("%s = %q, want %q", name, c[0], c[1])
		}
	}
	if cfg.ProxyBuffering {
		t.Error("ProxyBuffering is on")
	}
	if cfg.ProxyNextUpstreamTries == nil || *cfg.ProxyNextUpstreamTries != 0 {
		t.Errorf("ProxyNextUpstreamTries = %v, want 0", cfg.ProxyNextUpstreamTries)
	}
}

func TestParseIngressNginxProxyBuffers(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		annotations     map[string]string
		wantBuffers     string
		wantBufferSize  string
		wantWarningPart string
	}{
		{
			name:           "buffer size alone takes 4 buffers, as ingress-nginx",
			annotations:    map[string]string{IngressNginxProxyBufferSizeAnnotation: "8k"},
			wantBuffers:    "4 8k",
			wantBufferSize: "8k",
		},
		{
			name:           "buffers number alone takes 4k buffers, as ingress-nginx",
			annotations:    map[string]string{IngressNginxProxyBuffersNumberAnnotation: "16"},
			wantBuffers:    "16 4k",
			wantBufferSize: "4k",
		},
		{
			name: "nginx.org/proxy-buffers wins",
			annotations: map[string]string{
				IngressNginxProxyBufferSizeAnnotation: "8k",
				"nginx.org/proxy-buffers":             "2 32k",
			},
			wantBuffers:     "2 32k",
			wantBufferSize:  "",
			wantWarningPart: "nginx.org/proxy-buffers is set and takes precedence",
		},
		{
			name:            "a size in g is invalid",
			annotations:     map[string]string{IngressNginxProxyBufferSizeAnnotation: "1g"},
			wantWarningPart: IngressNginxProxyBufferSizeAnnotation + " is ignored",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := parseIngressNginxForTest(t, test.annotations, false)
			if cfg.ProxyBuffers != test.wantBuffers || cfg.ProxyBufferSize != test.wantBufferSize {
				t.Errorf("proxy_buffers %q, proxy_buffer_size %q, want %q and %q", cfg.ProxyBuffers, cfg.ProxyBufferSize, test.wantBuffers, test.wantBufferSize)
			}
			if test.wantWarningPart != "" && !hasWarningContaining(warnings, test.wantWarningPart) {
				t.Errorf("warnings %v lack %q", warnings, test.wantWarningPart)
			}
		})
	}
}

func TestParseIngressNginxProxyAnnotationsInvalid(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		IngressNginxProxyConnectTimeoutAnnotation:      "5s",
		IngressNginxProxyReadTimeoutAnnotation:         "-1",
		IngressNginxProxyBodySizeAnnotation:            "8 m",
		IngressNginxClientBodyBufferSizeAnnotation:     "1g",
		IngressNginxProxyBufferingAnnotation:           "true",
		IngressNginxProxyRequestBufferingAnnotation:    "On",
		IngressNginxProxyBuffersNumberAnnotation:       "0",
		IngressNginxProxyHTTPVersionAnnotation:         "2",
		IngressNginxProxyNextUpstreamAnnotation:        "error http_418",
		IngressNginxProxyNextUpstreamTimeoutAnnotation: "1m",
		IngressNginxProxyNextUpstreamTriesAnnotation:   "-3",
		IngressNginxProxySendTimeoutAnnotation:         "",
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want, _ := parseIngressNginxForTest(t, nil, false)
			got, warnings := parseIngressNginxForTest(t, map[string]string{name: value}, false)
			if !hasWarningContaining(warnings, name+" is ignored") {
				t.Errorf("warnings %v lack one for %s", warnings, name)
			}
			if got.ProxyConnectTimeout != want.ProxyConnectTimeout || got.ProxyReadTimeout != want.ProxyReadTimeout ||
				got.ProxySendTimeout != want.ProxySendTimeout || got.ClientMaxBodySize != want.ClientMaxBodySize ||
				got.ClientBodyBufferSize != want.ClientBodyBufferSize || got.ProxyBuffering != want.ProxyBuffering ||
				got.ProxyRequestBuffering != want.ProxyRequestBuffering || got.ProxyBuffers != want.ProxyBuffers ||
				got.ProxyHTTPVersion != want.ProxyHTTPVersion || got.ProxyNextUpstream != want.ProxyNextUpstream ||
				got.ProxyNextUpstreamTimeout != want.ProxyNextUpstreamTimeout || got.ProxyNextUpstreamTries != want.ProxyNextUpstreamTries {
				t.Errorf("an invalid %s changed the config", name)
			}
		})
	}
}

func TestParseIngressNginxProxyAnnotationsNginxOrgWins(t *testing.T) {
	t.Parallel()
	cfg, warnings := parseIngressNginxForTest(t, map[string]string{
		IngressNginxProxyReadTimeoutAnnotation: "5",
		"nginx.org/proxy-read-timeout":         "1m",
		IngressNginxProxyBodySizeAnnotation:    "8m",
		"nginx.org/client-max-body-size":       "2m",
	}, false)
	if cfg.ProxyReadTimeout != "1m" || cfg.ClientMaxBodySize != "2m" {
		t.Errorf("proxy_read_timeout %q, client_max_body_size %q, want 1m and 2m", cfg.ProxyReadTimeout, cfg.ClientMaxBodySize)
	}
	for _, part := range []string{"nginx.org/proxy-read-timeout is set", "nginx.org/client-max-body-size is set"} {
		if !hasWarningContaining(warnings, part) {
			t.Errorf("warnings %v lack %q", warnings, part)
		}
	}
}

func TestGenerateNginxCfgWithIngressNginxProxyAnnotations(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxProxyReadTimeoutAnnotation] = "300"
	ingEx.Ingress.Annotations[IngressNginxProxyRequestBufferingAnnotation] = "off"
	ingEx.Ingress.Annotations[IngressNginxProxyBufferingAnnotation] = "invalid"

	cfg, warnings := generateCanaryCfg(t, &ingEx, false)

	tea := findLocation(cfg, "/tea")
	if tea == nil {
		t.Fatal("the config lacks /tea")
	}
	if tea.ProxyReadTimeout != "300s" || tea.ProxyRequestBuffering != "off" {
		t.Errorf("/tea has proxy_read_timeout %q and proxy_request_buffering %q", tea.ProxyReadTimeout, tea.ProxyRequestBuffering)
	}
	if !hasWarningContaining(warnings[ingEx.Ingress], IngressNginxProxyBufferingAnnotation+" is ignored") {
		t.Errorf("warnings %v lack the invalid proxy-buffering", warnings)
	}
}

func locationPaths(locations []version1.Location) []string {
	var paths []string
	for _, loc := range locations {
		paths = append(paths, loc.Path)
	}
	return paths
}

func TestGenerateNginxCfgWithIngressNginxRewriteTarget(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Spec.Rules[0].HTTP.Paths[1].Path = "/tea(/|$)(.*)"
	ingEx.Ingress.Annotations[IngressNginxRewriteTargetAnnotation] = "/$2"
	ingEx.Ingress.Annotations[IngressNginxXForwardedPrefixAnnotation] = "/tea"

	cfg, warnings := generateCanaryCfg(t, &ingEx, false)
	if len(warnings[ingEx.Ingress]) != 0 {
		t.Errorf("warnings: %v", warnings)
	}

	locations := cfg.Servers[0].Locations
	// the longest path first, as ingress-nginx sorts them
	if got, want := locationPaths(locations), []string{"/tea(/|$)(.*)", "/coffee"}; !slices.Equal(got, want) {
		t.Fatalf("paths %v, want %v", got, want)
	}
	for _, loc := range locations {
		if !loc.CaseInsensitiveRegex || !loc.CaseInsensitiveRewrite || loc.RewriteTarget != "/$2" {
			t.Errorf("%s: regex %v, rewrite %v to %q", loc.Path, loc.CaseInsensitiveRegex, loc.CaseInsensitiveRewrite, loc.RewriteTarget)
		}
		if !slices.Contains(loc.ProxySetHeaders, version2.Header{Name: "X-Forwarded-Prefix", Value: "/tea"}) {
			t.Errorf("%s lacks X-Forwarded-Prefix: %v", loc.Path, loc.ProxySetHeaders)
		}
	}
}

func TestGenerateNginxCfgWithIngressNginxUseRegex(t *testing.T) {
	t.Parallel()
	exact := networking.PathTypeExact
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Spec.Rules[0].HTTP.Paths[0].PathType = &exact
	ingEx.Ingress.Annotations[IngressNginxUseRegexAnnotation] = "true"

	cfg, _ := generateCanaryCfg(t, &ingEx, false)

	for _, loc := range cfg.Servers[0].Locations {
		if !loc.CaseInsensitiveRegex || loc.RewriteTarget != "" || loc.CaseInsensitiveRewrite {
			t.Errorf("%s: regex %v, rewrite %v to %q", loc.Path, loc.CaseInsensitiveRegex, loc.CaseInsensitiveRewrite, loc.RewriteTarget)
		}
	}
}

func TestGenerateNginxCfgWithIngressNginxRewriteTargetOfThePath(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Annotations[IngressNginxRewriteTargetAnnotation] = "/tea"

	cfg, _ := generateCanaryCfg(t, &ingEx, false)

	// /tea is not rewritten, /coffee is, which makes both paths regular expressions
	tea, coffee := findLocation(cfg, "/tea"), findLocation(cfg, "/coffee")
	if tea.RewriteTarget != "" || !tea.CaseInsensitiveRegex {
		t.Errorf("/tea: rewrite to %q, regex %v", tea.RewriteTarget, tea.CaseInsensitiveRegex)
	}
	if coffee.RewriteTarget != "/tea" || !coffee.CaseInsensitiveRegex {
		t.Errorf("/coffee: rewrite to %q, regex %v", coffee.RewriteTarget, coffee.CaseInsensitiveRegex)
	}
}

func TestGenerateNginxCfgForMergeableIngressesWithIngressNginxUseRegex(t *testing.T) {
	t.Parallel()
	mergeableIngs := createMergeableCafeIngress()
	mergeableIngs.Minions[0].Ingress.Annotations[IngressNginxUseRegexAnnotation] = "true"

	cfg, _ := generateNginxCfgForMergeableIngresses(NginxCfgParams{
		mergeableIngs: mergeableIngs,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{},
	})

	// the use-regex of one minion makes the paths of the other one regular expressions too
	for _, path := range []string{"/coffee", "/tea"} {
		if loc := findLocation(cfg, path); loc == nil || !loc.CaseInsensitiveRegex {
			t.Errorf("%s is %+v", path, loc)
		}
	}
}

func TestParseIngressNginxRewriteAnnotations(t *testing.T) {
	t.Parallel()
	_, warnings := parseIngressNginxForTest(t, map[string]string{IngressNginxRewriteTargetAnnotation: "/$1?x=y"}, false)
	if !hasWarningContaining(warnings, IngressNginxRewriteTargetAnnotation+" is ignored") {
		t.Errorf("warnings %v lack the invalid rewrite-target", warnings)
	}
	cfg, warnings := parseIngressNginxForTest(t, map[string]string{
		IngressNginxRewriteTargetAnnotation: "/$1",
		RewriteTargetAnnotation:             "/x",
		IngressNginxUseRegexAnnotation:      "true",
		PathRegexAnnotation:                 "case_sensitive",
	}, false)
	if cfg.IngressNginxRewriteTarget != "" || cfg.IngressNginxUseRegex {
		t.Errorf("rewrite-target %q and use-regex %v win over nginx.org", cfg.IngressNginxRewriteTarget, cfg.IngressNginxUseRegex)
	}
	for _, part := range []string{RewriteTargetAnnotation + " is set", PathRegexAnnotation + " is set"} {
		if !hasWarningContaining(warnings, part) {
			t.Errorf("warnings %v lack %q", warnings, part)
		}
	}
}

func TestEnforceIngressNginxRegexKeepsInternalAndPathRegexLocations(t *testing.T) {
	t.Parallel()
	server := version1.Server{Locations: []version1.Location{
		{Path: "/_auth", Internal: true},
		{Path: "/a"},
		{Path: "/minion", MinionIngress: &version1.Ingress{Annotations: map[string]string{
			"nginx.org/mergeable-ingress-type": "minion",
			PathRegexAnnotation:                "case_sensitive",
		}}},
		{Path: "= /bb", UseRegex: true},
		{Path: "/ab"},
	}}
	enforceIngressNginxRegex(&server, nil)

	if got, want := locationPaths(server.Locations), []string{"/_auth", "/minion", "= /bb", "/ab", "/a"}; !slices.Equal(got, want) {
		t.Errorf("paths %v, want %v", got, want)
	}
	for _, loc := range server.Locations {
		want := loc.Path != "/_auth" && loc.Path != "/minion"
		if loc.CaseInsensitiveRegex != want {
			t.Errorf("%s: regex %v, want %v", loc.Path, loc.CaseInsensitiveRegex, want)
		}
	}
}

func TestParseIngressNginxRateLimitAnnotations(t *testing.T) {
	t.Parallel()
	ingEx := ingressNginxTestIngress(map[string]string{
		IngressNginxLimitRPSAnnotation:         "10",
		IngressNginxLimitRPMAnnotation:         "300",
		IngressNginxLimitConnectionsAnnotation: "2",
		IngressNginxLimitWhitelistAnnotation:   "192.168.0.1, 10.0.0.1/8,10.0.0.0/8",
		IngressNginxLimitRateAnnotation:        "512",
	})
	ingEx.Ingress.UID = "1234"
	cfg := NewDefaultConfigParams(context.Background(), false)
	warnings := parseIngressNginxAnnotations(ingEx, cfg, false, false)
	if len(warnings) != 0 {
		t.Errorf("warnings: %v", warnings)
	}
	rl := cfg.IngressNginxRateLimit
	if rl == nil {
		t.Fatal("no rate limit")
	}
	if rl.Name != "default_app_1234" || rl.ID != "ZGVmYXVsdF9hcHBfMTIzNA" || rl.Key != "$binary_remote_addr" || rl.ZoneSize != "5m" {
		t.Errorf("rate limit %+v", rl)
	}
	if rl.RPS != 10 || rl.RPSBurst != 50 || rl.RPM != 300 || rl.RPMBurst != 1500 || rl.Connections != 2 {
		t.Errorf("rate limit %+v, want the bursts 5 times the limits", rl)
	}
	if want := []string{"10.0.0.0/8", "192.168.0.1"}; !slices.Equal(rl.Allowlist, want) {
		t.Errorf("allowlist %v, want %v", rl.Allowlist, want)
	}
	if cfg.IngressNginxLimitRate != "512k" || cfg.IngressNginxLimitRateAfter != "" {
		t.Errorf("limit_rate %q, limit_rate_after %q", cfg.IngressNginxLimitRate, cfg.IngressNginxLimitRateAfter)
	}
}

func TestParseIngressNginxRateLimitAnnotationsIgnored(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		wantWarning string
	}{
		{
			name:        "an invalid allowlist drops the rate limit",
			annotations: map[string]string{IngressNginxLimitRPSAnnotation: "10", IngressNginxLimitAllowlistAnnotation: "10.0.0.300"},
			wantWarning: IngressNginxLimitAllowlistAnnotation + " is ignored",
		},
		{
			name:        "an invalid limit-connections drops the rate limit",
			annotations: map[string]string{IngressNginxLimitRPSAnnotation: "10", IngressNginxLimitConnectionsAnnotation: "many"},
			wantWarning: "the rate limit annotations",
		},
		{
			name:        "nginx.org/limit-req-rate wins",
			annotations: map[string]string{IngressNginxLimitRPSAnnotation: "10", "nginx.org/limit-req-rate": "5r/s"},
			wantWarning: "nginx.org/limit-req-rate is set",
		},
		{
			name:        "zero limits",
			annotations: map[string]string{IngressNginxLimitRPSAnnotation: "0", IngressNginxLimitBurstMultiplierAnnotation: "3"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := parseIngressNginxForTest(t, test.annotations, false)
			if cfg.IngressNginxRateLimit != nil {
				t.Errorf("rate limit %+v", cfg.IngressNginxRateLimit)
			}
			if test.wantWarning != "" && !hasWarningContaining(warnings, test.wantWarning) {
				t.Errorf("warnings %v lack %q", warnings, test.wantWarning)
			}
		})
	}

	cfg, warnings := parseIngressNginxForTest(t, map[string]string{IngressNginxLimitRPSAnnotation: "4", IngressNginxLimitBurstMultiplierAnnotation: "-1"}, false)
	if cfg.IngressNginxRateLimit == nil || cfg.IngressNginxRateLimit.RPSBurst != 20 || !hasWarningContaining(warnings, IngressNginxLimitBurstMultiplierAnnotation) {
		t.Errorf("an invalid multiplier: rate limit %+v, warnings %v", cfg.IngressNginxRateLimit, warnings)
	}
}

func TestGenerateNginxCfgWithIngressNginxRateLimit(t *testing.T) {
	t.Parallel()
	mergeableIngs := createMergeableCafeIngress()
	mergeableIngs.Minions[0].Ingress.Annotations[IngressNginxLimitRPSAnnotation] = "5"

	cfg, _ := generateNginxCfgForMergeableIngresses(NginxCfgParams{
		mergeableIngs: mergeableIngs,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{},
	})

	if len(cfg.IngressNginxRateLimits) != 1 {
		t.Fatalf("rate limits %+v, want the one of the minion", cfg.IngressNginxRateLimits)
	}
	limited := 0
	for _, server := range cfg.Servers {
		for _, loc := range server.Locations {
			if loc.IngressNginxRateLimit != nil {
				limited++
				if loc.IngressNginxRateLimit.Name != cfg.IngressNginxRateLimits[0].Name {
					t.Errorf("%s is limited by %s", loc.Path, loc.IngressNginxRateLimit.Name)
				}
			}
		}
	}
	if limited != 1 {
		t.Errorf("%d locations are limited, want the one of the minion", limited)
	}
}

func TestUsesClusterIP(t *testing.T) {
	t.Parallel()
	tests := []struct {
		annotations map[string]string
		want        bool
	}{
		{annotations: nil, want: false},
		{annotations: map[string]string{IngressNginxServiceUpstreamAnnotation: "true"}, want: true},
		{annotations: map[string]string{IngressNginxServiceUpstreamAnnotation: "True"}, want: true},
		{annotations: map[string]string{IngressNginxServiceUpstreamAnnotation: "yes"}, want: false},
		{annotations: map[string]string{UseClusterIPAnnotation: "true"}, want: true},
		{annotations: map[string]string{UseClusterIPAnnotation: "false", IngressNginxServiceUpstreamAnnotation: "true"}, want: false},
	}
	for _, test := range tests {
		ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Annotations: test.annotations}}
		if got := UsesClusterIP(ing); got != test.want {
			t.Errorf("UsesClusterIP(%v) = %v, want %v", test.annotations, got, test.want)
		}
	}
}

func TestParseIngressNginxServiceUpstream(t *testing.T) {
	t.Parallel()
	cfg, warnings := parseIngressNginxForTest(t, map[string]string{IngressNginxServiceUpstreamAnnotation: "true"}, false)
	if !cfg.UseClusterIP || len(warnings) != 0 {
		t.Errorf("UseClusterIP %v, warnings %v", cfg.UseClusterIP, warnings)
	}
	_, warnings = parseIngressNginxForTest(t, map[string]string{IngressNginxServiceUpstreamAnnotation: "yes"}, false)
	if !hasWarningContaining(warnings, IngressNginxServiceUpstreamAnnotation+" is ignored") {
		t.Errorf("warnings %v lack the invalid service-upstream", warnings)
	}
	_, warnings = parseIngressNginxForTest(t, map[string]string{IngressNginxServiceUpstreamAnnotation: "true", UseClusterIPAnnotation: "false"}, false)
	if !hasWarningContaining(warnings, UseClusterIPAnnotation+" is set and takes precedence") {
		t.Errorf("warnings %v lack the precedence of nginx.org/use-cluster-ip", warnings)
	}
}

func TestParseIngressNginxRedirect(t *testing.T) {
	t.Parallel()
	ing := func(annotations map[string]string) *ingressNginxAnnotations {
		return newIngressNginxAnnotations(&networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Name: "r", Namespace: "default", Annotations: annotations}})
	}
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		want        *version2.Return
		warnings    int
	}{
		{"none", map[string]string{}, nil, 0},
		{"permanent", map[string]string{"nginx.ingress.kubernetes.io/permanent-redirect": "https://example.org/target"},
			&version2.Return{Code: 301, Text: "https://example.org/target"}, 0},
		{"permanent with a code and a variable", map[string]string{
			"nginx.ingress.kubernetes.io/permanent-redirect":      "https://example.org$request_uri",
			"nginx.ingress.kubernetes.io/permanent-redirect-code": "308",
		}, &version2.Return{Code: 308, Text: "https://example.org$request_uri"}, 0},
		{"a bad code is the default", map[string]string{
			"nginx.ingress.kubernetes.io/permanent-redirect":      "https://example.org/",
			"nginx.ingress.kubernetes.io/permanent-redirect-code": "200",
		}, &version2.Return{Code: 301, Text: "https://example.org/"}, 1},
		{"temporal wins", map[string]string{
			"nginx.ingress.kubernetes.io/permanent-redirect": "https://example.org/p",
			"nginx.ingress.kubernetes.io/temporal-redirect":  "http://example.org/t",
		}, &version2.Return{Code: 302, Text: "http://example.org/t"}, 0},
		{"temporal with a code", map[string]string{
			"nginx.ingress.kubernetes.io/temporal-redirect":      "https://example.org/t",
			"nginx.ingress.kubernetes.io/temporal-redirect-code": "307",
		}, &version2.Return{Code: 307, Text: "https://example.org/t"}, 0},
		{"no injection", map[string]string{"nginx.ingress.kubernetes.io/permanent-redirect": "https://example.org/; return 200 x"}, nil, 1},
		{"no other scheme", map[string]string{"nginx.ingress.kubernetes.io/permanent-redirect": "javascript:alert(1)"}, nil, 1},
		{"no bare dollar", map[string]string{"nginx.ingress.kubernetes.io/permanent-redirect": "https://example.org/$"}, nil, 1},
	} {
		a := ing(tc.annotations)
		got := parseIngressNginxRedirect(a)
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
		n := 0
		for _, w := range a.warnings {
			n += len(w)
		}
		if n != tc.warnings {
			t.Errorf("%s: %d warnings %v, want %d", tc.name, n, a.warnings, tc.warnings)
		}
	}
}

func TestParseIngressNginxSnippets(t *testing.T) {
	t.Parallel()
	ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Name: "cafe", Namespace: "default", Annotations: map[string]string{
		IngressNginxServerSnippetAnnotation:        "add_header X-A 1;\nadd_header X-B 2;",
		IngressNginxConfigurationSnippetAnnotation: "add_header X-C 3;",
		"nginx.org/location-snippets":              "add_header X-D 4;",
	}}}
	a := newIngressNginxAnnotations(ing)
	server, location := parseIngressNginxSnippets(a, true)
	if !reflect.DeepEqual(server, []string{"add_header X-A 1;", "add_header X-B 2;"}) || location != nil {
		t.Errorf("server %q, location %q (nginx.org/location-snippets wins)", server, location)
	}
	a = newIngressNginxAnnotations(ing)
	if server, location = parseIngressNginxSnippets(a, false); server != nil || location != nil || len(a.warnings[ing]) != 2 {
		t.Errorf("without -enable-snippets: %q %q %v", server, location, a.warnings)
	}
}
