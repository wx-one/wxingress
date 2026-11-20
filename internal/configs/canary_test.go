package configs

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/nginx"
	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIsCanaryIngress(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"true":  true,
		"True":  true,
		"false": false,
		"":      false,
		"yes":   false,
	}
	for value, want := range tests {
		ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Annotations: map[string]string{CanaryAnnotation: value}}}
		if got := IsCanaryIngress(ing); got != want {
			t.Errorf("IsCanaryIngress(%q) = %v, want %v", value, got, want)
		}
	}
	if IsCanaryIngress(&networking.Ingress{}) {
		t.Error("an Ingress without annotations is a canary")
	}
}

func TestParseCanaryAnnotations(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		annotations map[string]string
		want        nginx.Canary
		wantErr     bool
	}{
		{
			name:        "defaults",
			annotations: map[string]string{CanaryAnnotation: "true"},
			want:        nginx.Canary{WeightTotal: 100},
		},
		{
			name: "everything",
			annotations: map[string]string{
				CanaryAnnotation:                "true",
				CanaryWeightAnnotation:          "5",
				CanaryWeightTotalAnnotation:     "1000",
				CanaryByHeaderAnnotation:        "X-Canary",
				CanaryByHeaderValueAnnotation:   "yes please",
				CanaryByHeaderPatternAnnotation: `^v[0-9]+\.beta$`,
				CanaryByCookieAnnotation:        "canary_cookie",
			},
			want: nginx.Canary{
				Weight:        5,
				WeightTotal:   1000,
				Header:        "X-Canary",
				HeaderValue:   "yes please",
				HeaderPattern: `^v[0-9]+\.beta$`,
				Cookie:        "canary_cookie",
			},
		},
		{name: "weight not a number", annotations: map[string]string{CanaryWeightAnnotation: "ten"}, wantErr: true},
		{name: "negative weight", annotations: map[string]string{CanaryWeightAnnotation: "-1"}, wantErr: true},
		{name: "zero weight total", annotations: map[string]string{CanaryWeightTotalAnnotation: "0"}, wantErr: true},
		{name: "header with a space", annotations: map[string]string{CanaryByHeaderAnnotation: "X Canary"}, wantErr: true},
		{name: "header too long", annotations: map[string]string{CanaryByHeaderAnnotation: strings.Repeat("x", 64)}, wantErr: true},
		{name: "header value with a quote", annotations: map[string]string{CanaryByHeaderValueAnnotation: `a"b`}, wantErr: true},
		{name: "header value with a backslash", annotations: map[string]string{CanaryByHeaderValueAnnotation: `a\b`}, wantErr: true},
		{name: "header pattern not a regex", annotations: map[string]string{CanaryByHeaderPatternAnnotation: "(unclosed"}, wantErr: true},
		{name: "cookie with a semicolon", annotations: map[string]string{CanaryByCookieAnnotation: "a;b"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Annotations: test.annotations}}
			got, err := ParseCanaryAnnotations(ing)
			if test.wantErr {
				if err == nil {
					t.Errorf("ParseCanaryAnnotations() returned no error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCanaryAnnotations() returned %v", err)
			}
			if got != test.want {
				t.Errorf("ParseCanaryAnnotations() = %+v, want %+v", got, test.want)
			}
		})
	}
}

func TestCanaryVariable(t *testing.T) {
	t.Parallel()
	tests := map[string]string{
		"default-cafe.example.com-tea-80": "$meta_canary_default_dcafe_pexample_pcom_dtea_d80",
		"a_b-c.d":                         "$meta_canary_a__b_dc_pd",
		"__":                              "$meta_canary_____",
		"plain":                           "$meta_canary_plain",
		"vs_ns_GW-1_be-2":                 "$meta_canary_vs__ns___ug_uw_d1__be_d2",
	}
	for upstream, want := range tests {
		if got := canaryVariable(upstream); got != want {
			t.Errorf("canaryVariable(%q) = %q, want %q", upstream, got, want)
		}
	}
}

// createCafeCanaryIngressEx returns a canary for /tea of cafe.example.com.
func createCafeCanaryIngressEx(annotations map[string]string) *IngressEx {
	all := map[string]string{
		"kubernetes.io/ingress.class": "nginx",
		CanaryAnnotation:              "true",
	}
	for k, v := range annotations {
		all[k] = v
	}
	return &IngressEx{
		Ingress: &networking.Ingress{
			ObjectMeta: meta_v1.ObjectMeta{Name: "cafe-canary", Namespace: "default", Annotations: all},
			Spec: networking.IngressSpec{
				Rules: []networking.IngressRule{{
					Host: "cafe.example.com",
					IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{
						Paths: []networking.HTTPIngressPath{{
							Path: "/tea",
							Backend: networking.IngressBackend{Service: &networking.IngressServiceBackend{
								Name: "tea-canary-svc",
								Port: networking.ServiceBackendPort{Number: 80},
							}},
						}},
					}},
				}},
			},
		},
		Endpoints:        map[string][]string{"tea-canary-svc80": {"10.0.0.3:80"}},
		ExternalNameSvcs: map[string]bool{},
		ValidHosts:       map[string]bool{"cafe.example.com": true},
	}
}

func generateCanaryCfg(t *testing.T, ingEx *IngressEx, upstreamAPI bool) (version1.IngressNginxConfig, Warnings) {
	t.Helper()
	return generateNginxCfg(NginxCfgParams{
		ingEx:         ingEx,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{UpstreamAPI: upstreamAPI},
	})
}

func findLocation(cfg version1.IngressNginxConfig, path string) *version1.Location {
	for _, server := range cfg.Servers {
		for i := range server.Locations {
			if server.Locations[i].Path == path {
				return &server.Locations[i]
			}
		}
	}
	return nil
}

func hasUpstream(cfg version1.IngressNginxConfig, name string) bool {
	return slices.ContainsFunc(cfg.Upstreams, func(u version1.Upstream) bool { return u.Name == name })
}

func TestGenerateNginxCfgWithCanary(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})}

	cfg, _ := generateCanaryCfg(t, &ingEx, true)

	const primary = "default-cafe-ingress-cafe.example.com-tea-svc-80"
	const canary = "default-cafe-canary-cafe.example.com-tea-canary-svc-80"

	if !hasUpstream(cfg, canary) {
		t.Errorf("the config lacks the canary upstream %s", canary)
	}
	tea := findLocation(cfg, "/tea")
	if tea == nil {
		t.Fatal("the config lacks /tea")
	}
	if want := "http://" + canaryVariable(primary); tea.ProxyPass != want {
		t.Errorf("/tea proxies to %q, want %q", tea.ProxyPass, want)
	}
	if tea.Upstream.Name != primary {
		t.Errorf("/tea has upstream %q, want %q", tea.Upstream.Name, primary)
	}
	if coffee := findLocation(cfg, "/coffee"); coffee == nil || coffee.ProxyPass != "http://default-cafe-ingress-cafe.example.com-coffee-svc-80" {
		t.Errorf("/coffee, which has no canary, is %+v", coffee)
	}

	want := map[string]nginx.Canary{primary: {Canary: canary, Weight: 10, WeightTotal: 100}}
	if len(cfg.Canaries) != 1 || cfg.Canaries[primary] != want[primary] {
		t.Errorf("Canaries = %+v, want %+v", cfg.Canaries, want)
	}
}

func TestGenerateNginxCfgIgnoresCanary(t *testing.T) {
	t.Parallel()
	exact := networking.PathTypeExact
	tests := []struct {
		name        string
		upstreamAPI bool
		change      func(primary *IngressEx, canary *IngressEx)
		wantWarning bool
	}{
		{
			name:        "without the upstream API",
			upstreamAPI: false,
			change:      func(_ *IngressEx, _ *IngressEx) {},
		},
		{
			name:        "another path type",
			upstreamAPI: true,
			change: func(_ *IngressEx, canary *IngressEx) {
				canary.Ingress.Spec.Rules[0].HTTP.Paths[0].PathType = &exact
			},
		},
		{
			name:        "another host",
			upstreamAPI: true,
			change: func(_ *IngressEx, canary *IngressEx) {
				canary.Ingress.Spec.Rules[0].Host = "tea.example.com"
			},
		},
		{
			name:        "rewrites",
			upstreamAPI: true,
			change: func(primary *IngressEx, _ *IngressEx) {
				primary.Ingress.Annotations["nginx.org/rewrites"] = "serviceName=tea-svc rewrite=/"
			},
			wantWarning: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ingEx := createCafeIngressEx()
			canaryEx := createCafeCanaryIngressEx(nil)
			ingEx.Canaries = []*IngressEx{canaryEx}
			test.change(&ingEx, canaryEx)

			cfg, warnings := generateCanaryCfg(t, &ingEx, test.upstreamAPI)

			if len(cfg.Canaries) != 0 {
				t.Errorf("Canaries = %+v, want none", cfg.Canaries)
			}
			if hasUpstream(cfg, "default-cafe-canary-cafe.example.com-tea-canary-svc-80") {
				t.Error("the config has the upstream of an ignored canary")
			}
			if tea := findLocation(cfg, "/tea"); tea == nil || strings.Contains(tea.ProxyPass, "$meta_canary_") {
				t.Errorf("/tea is %+v", tea)
			}
			if got := len(warnings[ingEx.Ingress]) > 0; got != test.wantWarning {
				t.Errorf("warnings = %v, want some: %v", warnings[ingEx.Ingress], test.wantWarning)
			}
		})
	}
}

func TestGenerateNginxCfgTakesFirstCanary(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	first := createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "1"})
	second := createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "2"})
	second.Ingress.Name = "cafe-canary-2"
	ingEx.Canaries = []*IngressEx{first, second}

	cfg, _ := generateCanaryCfg(t, &ingEx, true)

	got := cfg.Canaries["default-cafe-ingress-cafe.example.com-tea-svc-80"]
	if got.Canary != "default-cafe-canary-cafe.example.com-tea-canary-svc-80" || got.Weight != 1 {
		t.Errorf("the canary is %+v, want the first one", got)
	}
	if hasUpstream(cfg, "default-cafe-canary-2-cafe.example.com-tea-canary-svc-80") {
		t.Error("the config has the upstream of the second canary")
	}
}

func TestGenerateNginxCfgForMergeableIngressesWithCanary(t *testing.T) {
	t.Parallel()
	mergeableIngs := createMergeableCafeIngress()
	canaryEx := createCafeCanaryIngressEx(map[string]string{CanaryByHeaderAnnotation: "X-Canary"})
	for _, minion := range mergeableIngs.Minions {
		minion.Canaries = []*IngressEx{canaryEx}
	}

	cfg, _ := generateNginxCfgForMergeableIngresses(NginxCfgParams{
		mergeableIngs: mergeableIngs,
		BaseCfgParams: NewDefaultConfigParams(context.Background(), false),
		staticParams:  &StaticConfigParams{UpstreamAPI: true},
	})

	const primary = "default-cafe-ingress-tea-minion-cafe.example.com-tea-svc-80"
	if got := cfg.Canaries[primary]; got.Header != "X-Canary" {
		t.Errorf("Canaries = %+v, want one for %s", cfg.Canaries, primary)
	}
	if tea := findLocation(cfg, "/tea"); tea == nil || tea.ProxyPass != "http://"+canaryVariable(primary) {
		t.Errorf("/tea is %+v", tea)
	}
}

// canaryManager remembers configs to tell if they changed, and counts reloads and the calls of the API.
type canaryManager struct {
	*nginx.FakeManager
	configs map[string]string
	reloads int
	set     []string
	deleted []string
	updated []string
}

func newCanaryManager() *canaryManager {
	return &canaryManager{FakeManager: nginx.NewFakeManager("/etc/nginx"), configs: make(map[string]string)}
}

func (m *canaryManager) CreateConfig(name string, content []byte) (bool, error) {
	changed := m.configs[name] != string(content)
	m.configs[name] = string(content)
	return changed, nil
}

func (m *canaryManager) Reload(_ bool) error {
	m.reloads++
	return nil
}

func (m *canaryManager) SetCanary(primary string, canary nginx.Canary) error {
	m.set = append(m.set, primary+" "+canary.Canary)
	return nil
}

func (m *canaryManager) DeleteCanary(primary string) error {
	m.deleted = append(m.deleted, primary)
	return nil
}

func (m *canaryManager) UpdateServersInPlus(upstream string, _ []string, _ nginx.ServerConfig) error {
	m.updated = append(m.updated, upstream)
	return nil
}

func (m *canaryManager) reset() {
	m.reloads = 0
	m.set = nil
	m.deleted = nil
	m.updated = nil
}

func createCanaryConfigurator(t *testing.T, manager nginx.Manager) *Configurator {
	t.Helper()
	templateExecutor, err := version1.NewTemplateExecutor("version1/nginx.tmpl", "version1/nginx.ingress.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	templateExecutorV2, err := version2.NewTemplateExecutor("version2/nginx.virtualserver.tmpl", "version2/nginx.transportserver.tmpl", "version2/oidc.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	staticParams := createTestStaticConfigParams()
	staticParams.UpstreamAPI = true
	cnf := NewConfigurator(ConfiguratorParams{
		NginxManager:       manager,
		StaticCfgParams:    staticParams,
		Config:             NewDefaultConfigParams(context.Background(), false),
		MGMTCfgParams:      NewDefaultMGMTConfigParams(context.Background()),
		TemplateExecutor:   templateExecutor,
		TemplateExecutorV2: templateExecutorV2,
		IsDynamicUpstreams: true,
		NginxVersion:       nginx.NewVersion("nginx version: nginx/1.29.6"),
	})
	cnf.isReloadsEnabled = true
	return cnf
}

const (
	cafeTeaUpstream       = "default-cafe-ingress-cafe.example.com-tea-svc-80"
	cafeTeaCanaryUpstream = "default-cafe-canary-cafe.example.com-tea-canary-svc-80"
)

func TestConfiguratorCanaryLifecycle(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || len(manager.set) != 0 || len(manager.deleted) != 0 {
		t.Fatalf("without a canary: reloads %d, set %v, deleted %v", manager.reloads, manager.set, manager.deleted)
	}

	// a new canary: the config gets its upstream, so a reload, then the rule
	manager.reset()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "0"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("new canary: reloads = %d, want 1", manager.reloads)
	}
	if want := []string{cafeTeaUpstream + " " + cafeTeaCanaryUpstream}; !slices.Equal(manager.set, want) {
		t.Errorf("new canary: set %v, want %v", manager.set, want)
	}

	// only the weight changes: the rule, without a reload
	manager.reset()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "100"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 {
		t.Errorf("weight change: reloads = %d, want 0", manager.reloads)
	}
	if len(manager.set) != 1 {
		t.Errorf("weight change: set %v, want one rule", manager.set)
	}
	if got := cnf.appliedCanaries[cafeTeaUpstream].Weight; got != 100 {
		t.Errorf("weight change: applied weight %d, want 100", got)
	}

	// the same again: nothing for the module, and no reload
	manager.reset()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if len(manager.set) != 0 || manager.reloads != 0 || len(manager.updated) != 0 {
		t.Errorf("no change: set %v, reloads %d, updated %v, want none", manager.set, manager.reloads, manager.updated)
	}

	// endpoints of the canary go through the API as well - only those that changed
	manager.reset()
	ingEx.Canaries[0].Endpoints["tea-canary-svc80"] = []string{"10.0.0.3:80", "10.0.0.4:80"}
	if _, err := cnf.UpdateEndpoints([]*IngressEx{&ingEx}); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || !slices.Equal(manager.updated, []string{cafeTeaCanaryUpstream}) {
		t.Errorf("endpoints: reloads %d, updated %v, want no reload and %s", manager.reloads, manager.updated, cafeTeaCanaryUpstream)
	}

	// the canary goes away: a reload, then the rule is deleted
	manager.reset()
	ingEx.Canaries = nil
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("removed canary: reloads = %d, want 1", manager.reloads)
	}
	if want := []string{cafeTeaUpstream}; !slices.Equal(manager.deleted, want) {
		t.Errorf("removed canary: deleted %v, want %v", manager.deleted, want)
	}
}

func TestConfiguratorDeletesCanaryWithIngress(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)

	ingEx := createCafeIngressEx()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(nil)}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}

	manager.reset()
	if err := cnf.DeleteIngress("default/cafe-ingress", false); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || !slices.Equal(manager.deleted, []string{cafeTeaUpstream}) {
		t.Errorf("reloads %d, deleted %v", manager.reloads, manager.deleted)
	}
}

func TestConfiguratorSetsCanariesWhenReloadsAreEnabled(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.isReloadsEnabled = false

	ingEx := createCafeIngressEx()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(nil)}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if len(manager.set) != 0 {
		t.Fatalf("set %v before nginx runs the config", manager.set)
	}

	cnf.EnableReloads()
	if err := cnf.Reload(nginx.ReloadForOtherUpdate); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(manager.set, []string{cafeTeaUpstream + " " + cafeTeaCanaryUpstream}) {
		t.Errorf("set %v after the first reload", manager.set)
	}
}

// A canary of an upstream (-ingress-nginx-defaults) is the canary of every path whose backend is that upstream,
// whatever its host and path: as ingress-nginx's alternative backend of a backend.
func TestGenerateNginxCfgWithCanaryPerUpstream(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	canaryEx := createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})
	// the canary's own host and path are another's: what counts is the upstream it is the canary of
	canaryEx.Ingress.Spec.Rules[0].Host = "other.example.com"
	canaryEx.CanaryOf = map[string]networking.IngressBackend{"default-tea-svc-80": canaryEx.Ingress.Spec.Rules[0].HTTP.Paths[0].Backend}
	ingEx.Canaries = []*IngressEx{canaryEx}

	cfg, _ := generateCanaryCfg(t, &ingEx, true)

	const primary = "default-cafe-ingress-cafe.example.com-tea-svc-80"
	// named by the host of the location, so that each config has its own
	const canary = "default-cafe-canary-cafe.example.com-tea-canary-svc-80"
	if !hasUpstream(cfg, canary) {
		t.Errorf("the config lacks the canary upstream %s", canary)
	}
	if tea := findLocation(cfg, "/tea"); tea == nil || tea.ProxyPass != "http://"+canaryVariable(primary) {
		t.Errorf("/tea is %+v", tea)
	}
	if coffee := findLocation(cfg, "/coffee"); coffee == nil || strings.Contains(coffee.ProxyPass, "$meta_canary_") {
		t.Errorf("/coffee, of another Service, is %+v", coffee)
	}
	if got := cfg.Canaries[primary]; got.Canary != canary || got.Weight != 10 {
		t.Errorf("Canaries = %+v", cfg.Canaries)
	}
}
