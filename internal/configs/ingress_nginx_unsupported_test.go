package configs

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Every ingress-nginx annotation the code of the Ingress path names is in
// ingressNginxRead, so that checkIngressNginxUnsupported does not call one
// that is read unsupported.
func TestIngressNginxReadIsComplete(t *testing.T) {
	t.Parallel()
	literal := regexp.MustCompile(`"nginx\.ingress\.kubernetes\.io/([a-z0-9-]+)"|IngressNginxAnnotationPrefix \+ "([a-z0-9-]+)"`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "gateway.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			name := m[1] + m[2]
			if !ingressNginxRead[IngressNginxAnnotationPrefix+name] {
				t.Errorf("%s names %s, which is not in ingressNginxRead", f, name)
			}
		}
	}
}

func unsupportedOf(annotations map[string]string) (*ConfigParams, []string) {
	ing := &networking.Ingress{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "ing", Annotations: annotations}}
	a := newIngressNginxAnnotations(ing)
	cfg := &ConfigParams{}
	parseIngressNginxSourceRanges(a, cfg)
	checkIngressNginxUnsupported(a, cfg)
	return cfg, a.warnings[ing]
}

func TestIngressNginxUnsupportedAnnotations(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		denied      bool
		warning     string
	}{
		{"read ones say nothing", map[string]string{p + "proxy-body-size": "8m", p + "canary": "false"}, false, ""},
		{"an unsupported one is warned about", map[string]string{p + "http2-push-preload": "true"}, false, p + "http2-push-preload is not supported by wxingress and is ignored"},
		{"a typo as well", map[string]string{p + "proxy-body-sise": "8m"}, false, "proxy-body-sise is not supported"},
		{"ModSecurity on denies", map[string]string{p + "enable-modsecurity": "true"}, true, "ModSecurity"},
		{"ModSecurity off does not", map[string]string{p + "enable-modsecurity": "false"}, false, "enable-modsecurity is not supported"},
		{"other prefixes are not looked at", map[string]string{"example.com/enable-cors": "true"}, false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := unsupportedOf(tt.annotations)
			if cfg.IngressNginxDenied != tt.denied {
				t.Errorf("denied %v, want %v (%v)", cfg.IngressNginxDenied, tt.denied, warnings)
			}
			if tt.warning == "" {
				if len(warnings) != 0 {
					t.Errorf("warnings %v", warnings)
				}
				return
			}
			if !slices.ContainsFunc(warnings, func(w string) bool { return strings.Contains(w, tt.warning) }) {
				t.Errorf("no warning with %q in %v", tt.warning, warnings)
			}
		})
	}
}

func TestIngressNginxSourceRanges(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	tests := []struct {
		name        string
		annotations map[string]string
		rules       []string
		denied      bool
	}{
		{"allowlist", map[string]string{p + "allowlist-source-range": "10.0.0.0/8, 192.0.2.1"}, []string{"allow 10.0.0.0/8", "allow 192.0.2.1", "deny all"}, false},
		{"whitelist, the old name", map[string]string{p + "whitelist-source-range": "0.0.0.0/0"}, []string{"allow 0.0.0.0/0", "deny all"}, false},
		{"allowlist wins over whitelist", map[string]string{p + "allowlist-source-range": "10.0.0.0/8", p + "whitelist-source-range": "0.0.0.0/0"}, []string{"allow 10.0.0.0/8", "deny all"}, false},
		{"denylist", map[string]string{p + "denylist-source-range": "198.51.100.0/24"}, []string{"deny 198.51.100.0/24"}, false},
		{"both: denied first", map[string]string{p + "denylist-source-range": "10.1.0.0/16", p + "allowlist-source-range": "10.0.0.0/8"}, []string{"deny 10.1.0.0/16", "allow 10.0.0.0/8", "deny all"}, false},
		{"IPv6", map[string]string{p + "allowlist-source-range": "2001:db8::/32"}, []string{"allow 2001:db8::/32", "deny all"}, false},
		{"an invalid range denies", map[string]string{p + "allowlist-source-range": "10.0.0.0/8,nonsense"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			cfg, warnings := unsupportedOf(tt.annotations)
			if !slices.Equal(cfg.IngressNginxAccess, tt.rules) {
				t.Errorf("rules %q, want %q", cfg.IngressNginxAccess, tt.rules)
			}
			if cfg.IngressNginxDenied != tt.denied {
				t.Errorf("denied %v, want %v (%v)", cfg.IngressNginxDenied, tt.denied, warnings)
			}
			for _, w := range warnings {
				if strings.Contains(w, "source-range is not supported") {
					t.Errorf("a source range is called unsupported: %s", w)
				}
			}
		})
	}
}

func TestUnsupportedOnRoutes(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	s := ParseGatewaySettings("default", "route", map[string]string{
		p + "proxy-read-timeout": "30", p + "enable-cors": "true", p + "whitelist-source-range": "10.0.0.0/8",
	}, false)
	all := strings.Join(s.Warnings, "\n")
	for _, want := range []string{p + "enable-cors is not supported on Gateway API routes and is ignored", p + "whitelist-source-range is not supported on Gateway API routes: the rules of the route answer 503"} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in %q", want, all)
		}
	}
	if strings.Contains(all, "proxy-read-timeout") {
		t.Errorf("a read annotation is flagged: %q", all)
	}
}
