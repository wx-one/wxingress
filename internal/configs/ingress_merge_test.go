package configs

import (
	"context"
	"slices"
	"testing"

	networking "k8s.io/api/networking/v1"
)

// mergedCafeIngressEx returns an Ingress of the host of the cafe Ingress with
// the paths, in another namespace and without TLS, merged as the controller merges it.
func mergedCafeIngressEx(name string, annotations map[string]string, paths ...string) *IngressEx {
	ingEx := createCafeIngressEx()
	ingEx.Ingress.Name = name
	ingEx.Ingress.Namespace = "other"
	ingEx.Ingress.Spec.TLS = nil
	ingEx.SecretRefs = nil
	for k, v := range annotations {
		ingEx.Ingress.Annotations[k] = v
	}
	prefix := networking.PathTypeImplementationSpecific
	var httpPaths []networking.HTTPIngressPath
	for _, path := range paths {
		httpPaths = append(httpPaths, networking.HTTPIngressPath{
			Path:     path,
			PathType: &prefix,
			Backend: networking.IngressBackend{Service: &networking.IngressServiceBackend{
				Name: "direct-svc",
				Port: networking.ServiceBackendPort{Number: 8181},
			}},
		})
	}
	ingEx.Ingress.Spec.Rules[0].HTTP.Paths = httpPaths
	ingEx.Endpoints = map[string][]string{"direct-svc8181": {"10.0.0.3:8181"}}
	return &ingEx
}

func TestGenerateNginxCfgWithMergedIngresses(t *testing.T) {
	t.Parallel()
	holder := createCafeIngressEx()
	holder.Merged = []*IngressEx{mergedCafeIngressEx("direct", map[string]string{
		IngressNginxProxyReadTimeoutAnnotation: "600",
	}, "/api", "/tea")}

	cfg, warnings := generateNginxCfg(NginxCfgParams{
		ingEx:         &holder,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{},
	})

	api := findLocation(cfg, "/api")
	if api == nil {
		t.Fatalf("the server lacks the path of the merged Ingress: %v", cfg.Servers[0].Locations)
	}
	// its annotations apply to its own paths, not to those of the holder
	if api.ProxyReadTimeout != "600s" || api.MinionIngress == nil || api.MinionIngress.Name != "direct" {
		t.Errorf("/api: proxy_read_timeout %q, Ingress %v", api.ProxyReadTimeout, api.MinionIngress)
	}
	if coffee := findLocation(cfg, "/coffee"); coffee == nil || coffee.ProxyReadTimeout != "60s" {
		t.Errorf("/coffee: %+v", coffee)
	}

	// the path the holder has already stays the holder's
	var tea int
	for _, loc := range cfg.Servers[0].Locations {
		if loc.Path == "/tea" {
			tea++
			if loc.ServiceName != "tea-svc" {
				t.Errorf("/tea goes to %s", loc.ServiceName)
			}
		}
	}
	if tea != 1 {
		t.Errorf("%d locations /tea", tea)
	}
	var directWarnings []string
	for obj, msgs := range warnings {
		if ing, ok := obj.(*networking.Ingress); ok && ing.Name == "direct" {
			directWarnings = msgs
		}
	}
	if !hasWarningContaining(directWarnings, "path /tea of host cafe.example.com is ignored: Ingress default/cafe-ingress has it already") {
		t.Errorf("warnings of the merged Ingress: %v", directWarnings)
	}
	if !hasUpstream(cfg, "other_direct_cafe.example.com_direct-svc_8181") {
		t.Errorf("the upstreams %v lack the one of the merged Ingress", cfg.Upstreams)
	}
}

func TestGenerateNginxCfgWithMergedIngressesTakesTheirTLSAndRegex(t *testing.T) {
	t.Parallel()
	// the holder has neither TLS nor regular expressions, the merged Ingress has both
	holder := mergedCafeIngressEx("direct", map[string]string{IngressNginxUseRegexAnnotation: "true"}, "/api/.*")
	holder.Ingress.Namespace = "default"
	chart := createCafeIngressEx()
	chart.Ingress.Name = "chart"
	chart.Ingress.Spec.Rules[0].HTTP.Paths = chart.Ingress.Spec.Rules[0].HTTP.Paths[:1]
	chart.Ingress.Spec.Rules[0].HTTP.Paths[0].Path = "/"
	holder.Merged = []*IngressEx{&chart}

	cfg, _ := generateNginxCfg(NginxCfgParams{
		ingEx:         holder,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{},
	})

	server := cfg.Servers[0]
	if !server.SSL || server.SSLCertificate != "/etc/nginx/secrets/default-cafe-secret" {
		t.Errorf("the server has TLS %v with %q, want the certificate of the merged Ingress", server.SSL, server.SSLCertificate)
	}
	// use-regex of one Ingress of the host makes every path of it a regular expression, the longest first
	if len(server.Locations) != 2 || server.Locations[0].Path != "/api/.*" || !server.Locations[1].CaseInsensitiveRegex || server.Locations[1].Path != "/" {
		t.Errorf("locations: %+v", server.Locations)
	}
}

func TestGenerateNginxCfgSplitsPrefixPathsAsIngressNginx(t *testing.T) {
	t.Parallel()
	prefix := networking.PathTypePrefix
	exact := networking.PathTypeExact
	tests := []struct {
		name        string
		annotations map[string]string
		paths       []networking.HTTPIngressPath
		want        []string
	}{
		{
			name:  "a Prefix path matches itself and what is below it",
			paths: []networking.HTTPIngressPath{{Path: "/tea", PathType: &prefix}},
			want:  []string{"/tea/", "= /tea"},
		},
		{
			name:  "the root and a path that ends in / stay",
			paths: []networking.HTTPIngressPath{{Path: "/", PathType: &prefix}, {Path: "/tea/", PathType: &prefix}},
			want:  []string{"/", "/tea/"},
		},
		{
			name:  "an Exact path of the same path is not doubled",
			paths: []networking.HTTPIngressPath{{Path: "/tea", PathType: &prefix}, {Path: "/tea", PathType: &exact}},
			want:  []string{"/tea/", "= /tea"},
		},
		{
			name:  "an ImplementationSpecific path stays",
			paths: []networking.HTTPIngressPath{{Path: "/tea"}},
			want:  []string{"/tea"},
		},
		{
			name:        "a regular expression stays",
			annotations: map[string]string{IngressNginxUseRegexAnnotation: "true"},
			paths:       []networking.HTTPIngressPath{{Path: "/tea", PathType: &prefix}},
			want:        []string{"/tea"},
		},
		{
			name:        "a rewritten path stays",
			annotations: map[string]string{"nginx.org/rewrites": "serviceName=tea-svc rewrite=/"},
			paths:       []networking.HTTPIngressPath{{Path: "/tea", PathType: &prefix}},
			want:        []string{"/tea"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ingEx := createCafeIngressEx()
			for k, v := range test.annotations {
				ingEx.Ingress.Annotations[k] = v
			}
			for i := range test.paths {
				test.paths[i].Backend = networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: "tea-svc", Port: networking.ServiceBackendPort{Number: 80}}}
			}
			ingEx.Ingress.Spec.Rules[0].HTTP.Paths = test.paths
			cfg, _ := generateNginxCfg(NginxCfgParams{
				ingEx:         &ingEx,
				BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
				staticParams:  &StaticConfigParams{},
			})
			var paths []string
			for _, loc := range cfg.Servers[0].Locations {
				paths = append(paths, loc.Path)
			}
			if !slices.Equal(paths, test.want) {
				t.Errorf("locations %v, want %v", paths, test.want)
			}
		})
	}
}
