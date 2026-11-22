package configs

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/gateway/extras"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// gatewayVirtualServer is a VirtualServer as the Gateway layer makes it: a
// route whose match proxies with extras, and a redirect.
func gatewayVirtualServer(name string) *conf_v1.VirtualServer {
	e := &extras.Extras{
		HTTP: []string{"map $http_x_add $wxgw_x_add_x_add {\n    \"\" \"\";\n    default \"$http_x_add,\";\n}"},
		Actions: map[string]*extras.Action{
			"a1": {
				SetHeaders:  []extras.Header{{Name: "Host", Value: "${wxgw_x_host}"}, {Name: "X-Add", Value: "${wxgw_x_add_x_add}v"}, {Name: "X-Remove", Value: ""}},
				HideHeaders: []string{"X-Upstream"},
				AddHeaders:  []extras.Header{{Name: "X-Response", Value: "r"}},
				Rewrites:    []string{"^ $wxgw_x_rewrite_1 break"},
			},
			"a2": {Return: &version2.Return{Code: 303, Text: "https://example.org$request_uri"}},
		},
	}
	marked := func(key string) *conf_v1.Action {
		return &conf_v1.Action{Proxy: &conf_v1.ActionProxy{
			Upstream:       "be-1",
			RequestHeaders: &conf_v1.ProxyRequestHeaders{Set: []conf_v1.Header{{Name: extras.MarkerHeader, Value: key}}},
		}}
	}
	return &conf_v1.VirtualServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "infra",
			Name:        name,
			Annotations: map[string]string{extras.Annotation: e.Encode()},
		},
		Spec: conf_v1.VirtualServerSpec{
			Host:      "gw.example.com",
			Upstreams: []conf_v1.Upstream{{Name: "be-1", Service: "app/a", Port: 80}},
			Routes: []conf_v1.Route{
				{
					Path: "/coffee/",
					Matches: []conf_v1.Match{{
						Conditions: []conf_v1.Condition{{Header: "X-Version", Value: "2"}},
						Action:     marked("a1"),
					}},
					Action: &conf_v1.Action{Return: &conf_v1.ActionReturn{Code: 404, Type: "text/plain", Body: "Not Found"}},
				},
				{
					Path:   "/old",
					Action: &conf_v1.Action{Redirect: &conf_v1.ActionRedirect{URL: extras.MarkerURL("a2"), Code: 302}},
				},
			},
		},
	}
}

func TestGatewayExtrasAreRendered(t *testing.T) {
	t.Parallel()
	cnf := createTestConfigurator(t)
	executor, err := version2.NewTemplateExecutor("version2/nginx.virtualserver.tmpl", "version2/nginx.transportserver.tmpl", "version2/oidc.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	vsEx := &VirtualServerEx{
		VirtualServer: gatewayVirtualServer("GW-0123456789abcdef"),
		Endpoints:     map[string][]string{"app/a:80": {"10.0.0.1:8080"}},
	}
	cfg, warnings := cnf.generateVirtualServerConfig(vsEx)
	if len(warnings) != 0 {
		t.Fatalf("warnings %v", warnings)
	}
	content, err := executor.ExecuteVirtualServerTemplate(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	text := string(content)
	for _, want := range []string{
		`map $http_x_add $wxgw_x_add_x_add {`,
		`proxy_set_header Host "${wxgw_x_host}";`,
		`proxy_set_header X-Add "${wxgw_x_add_x_add}v";`,
		`proxy_set_header X-Remove "";`,
		`proxy_hide_header X-Upstream;`,
		`add_header X-Response "r" always;`,
		`rewrite ^ $wxgw_x_rewrite_1 break;`,
		`return 303 "https://example.org$request_uri";`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the config has no %q", want)
		}
	}
	for _, unwanted := range []string{extras.MarkerHeader, "action.wxingress.invalid", `proxy_set_header Host "$host"`} {
		if strings.Contains(text, unwanted) {
			t.Errorf("the config still has %q", unwanted)
		}
	}
	// the match's internal location proxies the rewritten URI
	if strings.Contains(text, "be-1$request_uri") {
		t.Error("the rewritten URI does not go to the upstream")
	}
}

func TestExtrasOfVirtualServersFromTheAPIAreIgnored(t *testing.T) {
	t.Parallel()
	cnf := createTestConfigurator(t)
	vs := gatewayVirtualServer("cafe")
	vsEx := &VirtualServerEx{VirtualServer: vs, Endpoints: map[string][]string{"app/a:80": {"10.0.0.1:8080"}}}
	cfg, _ := cnf.generateVirtualServerConfig(vsEx)
	if len(cfg.HTTPSnippets) != 0 {
		t.Errorf("a VirtualServer from the API got the extras of its annotation: %v", cfg.HTTPSnippets)
	}
}

func TestGatewayBalancersAndSettings(t *testing.T) {
	t.Parallel()
	s := ParseGatewaySettings("app", "r", map[string]string{
		"nginx.ingress.kubernetes.io/proxy-connect-timeout": "5",
		"nginx.ingress.kubernetes.io/proxy-buffering":       "off",
		"nginx.ingress.kubernetes.io/upstream-hash-by":      "$remote_addr",
	}, false)
	if s.Upstream.ProxyConnectTimeout != "5s" || s.Upstream.ProxyBuffering == nil || *s.Upstream.ProxyBuffering ||
		s.Upstream.LBMethod != "hash $remote_addr consistent" || !s.Balancing {
		t.Errorf("settings %+v balancing %v", s.Upstream, s.Balancing)
	}
	if b, ok := s.Balancer("app-a-80", []string{"/"}); !ok || b.Algorithm != "chash" || b.HashBy != "$remote_addr" {
		t.Errorf("balancer %+v", b)
	}
	if tls := ParseGatewaySettings("app", "r", map[string]string{IngressNginxBackendProtocolAnnotation: "HTTPS"}, false); !tls.Upstream.TLS.Enable || tls.Empty() {
		t.Errorf("backend-protocol HTTPS: %+v", tls.Upstream)
	}
	if bad := ParseGatewaySettings("app", "r", map[string]string{IngressNginxBackendProtocolAnnotation: "FCGI"}, false); bad.Upstream.TLS.Enable || len(bad.Warnings) != 1 {
		t.Errorf("backend-protocol FCGI: %+v %q", bad.Upstream, bad.Warnings)
	}
	if !ParseGatewaySettings("app", "r", map[string]string{}, false).Empty() {
		t.Error("no annotations are settings")
	}

	// a VirtualServer with a balancer rule in its extras: its upstream says
	// meta_balancer, and the rule is the configurator's under the nginx name
	vs := gatewayVirtualServer("GW-0123456789abcdef")
	e, _ := extras.Decode(vs.Annotations[extras.Annotation])
	e.Balancers = map[string]json.RawMessage{"be-1": json.RawMessage(`{"algorithm":"ewma","backend":"app-a-80"}`)}
	vs.Annotations[extras.Annotation] = e.Encode()
	cnf := createTestConfigurator(t)
	cfg, _ := cnf.generateVirtualServerConfig(&VirtualServerEx{VirtualServer: vs, Endpoints: map[string][]string{"app/a:80": {"10.0.0.1:8080"}}})
	executor, err := version2.NewTemplateExecutor("version2/nginx.virtualserver.tmpl", "version2/nginx.transportserver.tmpl", "version2/oidc.tmpl")
	if err != nil {
		t.Fatal(err)
	}
	content, err := executor.ExecuteVirtualServerTemplate(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "meta_balancer;") {
		t.Errorf("no meta_balancer:\n%s", content)
	}
	rules := gatewayBalancers(vs)
	if r, ok := rules["vs_infra_GW-0123456789abcdef_be-1"]; !ok || r.Algorithm != "ewma" {
		t.Errorf("rules %+v", rules)
	}
}

func TestGatewaySettingsLimitsCanaryAndSnippets(t *testing.T) {
	t.Parallel()
	s := ParseGatewaySettings("app", "r", map[string]string{
		IngressNginxLimitConnectionsAnnotation: "2",
		IngressNginxLimitRPSAnnotation:         "3",
		IngressNginxLimitRateAnnotation:        "10",
		IngressNginxLimitAllowlistAnnotation:   "10.0.0.0/8",
		CanaryByCookieAnnotation:               "beta",
		CanaryByHeaderAnnotation:               "X-Canary",
		CanaryByHeaderValueAnnotation:          "yes",
		IngressNginxServerSnippetAnnotation:    "add_header X-Server 1;",
		"nginx.org/location-snippets":          "add_header X-Location 1;",
	}, true)
	if s.Empty() || !s.Limits() {
		t.Fatalf("settings %+v", s)
	}
	http, directives := s.RateLimit("wxgw_x_limit_app_r")
	for _, want := range []string{"zone=wxgw_x_limit_app_r_conn:5m;", "zone=wxgw_x_limit_app_r_rps:5m rate=3r/s;", "\t10.0.0.0/8 1;"} {
		if !strings.Contains(http, want) {
			t.Errorf("no %q in\n%s", want, http)
		}
	}
	if want := []string{"limit_conn wxgw_x_limit_app_r_conn 2;", "limit_conn_status 503;", "limit_req zone=wxgw_x_limit_app_r_rps burst=15 nodelay;", "limit_req_status 503;", "limit_rate 10k;"}; !slices.Equal(directives, want) {
		t.Errorf("directives %q, want %q", directives, want)
	}
	if c := s.CanaryBy; c == nil || c.Cookie != "beta" || c.Header != "X-Canary" || c.HeaderValue != "yes" || c.Weight != 0 {
		t.Errorf("canary %+v", s.CanaryBy)
	}
	if !slices.Equal(s.ServerSnippets, []string{"add_header X-Server 1;"}) || !slices.Equal(s.LocationSnippets, []string{"add_header X-Location 1;"}) {
		t.Errorf("snippets %q %q", s.ServerSnippets, s.LocationSnippets)
	}

	off := ParseGatewaySettings("app", "r", map[string]string{
		IngressNginxConfigurationSnippetAnnotation: "add_header X 1;",
		"nginx.org/server-snippets":                "add_header Y 1;",
	}, false)
	if len(off.ServerSnippets) != 0 || len(off.LocationSnippets) != 0 || len(off.Warnings) != 2 {
		t.Errorf("snippets without -enable-snippets: %q %q %q", off.ServerSnippets, off.LocationSnippets, off.Warnings)
	}
	if bad := ParseGatewaySettings("app", "r", map[string]string{CanaryByCookieAnnotation: "a b"}, false); bad.CanaryBy != nil || len(bad.Warnings) != 1 {
		t.Errorf("an invalid cookie: %+v", bad)
	}
}

func TestGatewayCanaryGoesToTheModule(t *testing.T) {
	t.Parallel()
	vs := gatewayVirtualServer("GW-0123456789abcdef")
	vs.Spec.Upstreams = append(vs.Spec.Upstreams, conf_v1.Upstream{Name: "be-2", Service: "app/b", Port: 80})
	e, _ := extras.Decode(vs.Annotations[extras.Annotation])
	e.Actions["a1"].Canary = &extras.Canary{Primary: "be-1", Upstream: "be-2", Weight: 1, WeightTotal: 4}
	e.Actions["a1"].Rewrites = nil
	vs.Annotations[extras.Annotation] = e.Encode()
	cnf := createTestConfigurator(t)
	cfg, _ := cnf.generateVirtualServerConfig(&VirtualServerEx{VirtualServer: vs, Endpoints: map[string][]string{"app/a:80": {"10.0.0.1:8080"}, "app/b:80": {"10.0.0.2:8080"}}})
	found := false
	for _, l := range cfg.Server.Locations {
		if strings.HasPrefix(l.ProxyPass, "http://$meta_canary_") {
			found = true
		}
	}
	if !found {
		t.Errorf("no location proxies to the canary variable: %+v", cfg.Server.Locations)
	}
	c := gatewayCanaries(vs)
	if r, ok := c["vs_infra_GW-0123456789abcdef_be-1"]; !ok || r.Canary != "vs_infra_GW-0123456789abcdef_be-2" || r.Weight != 1 || r.WeightTotal != 4 {
		t.Errorf("canaries %+v", c)
	}
}

// TestRestrictiveAnnotationsOfARouteAreReported: the annotations of a route
// that restrict who gets an answer, which a route does not take, are
// reported, so that its rules answer 503 rather than serve without them.
func TestRestrictiveAnnotationsOfARouteAreReported(t *testing.T) {
	t.Parallel()
	p := IngressNginxAnnotationPrefix
	s := ParseGatewaySettings("app", "r", map[string]string{
		p + "auth-url": "https://auth.example.com", p + "whitelist-source-range": "10.0.0.0/8", p + "satisfy": "any",
		p + "enable-modsecurity": "false", p + "enable-cors": "true", p + "proxy-read-timeout": "30",
	}, false)
	want := []string{p + "auth-url", p + "satisfy", p + "whitelist-source-range"}
	if !slices.Equal(s.Restricted, want) {
		t.Errorf("restricted %q, want %q", s.Restricted, want)
	}
	if s.Empty() {
		t.Error("settings with restrictions are empty")
	}
	if only := ParseGatewaySettings("app", "r", map[string]string{p + "denylist-source-range": "10.0.0.0/8"}, false); only.Empty() {
		t.Error("settings with a deny list alone are empty")
	}
	// what restricts nothing on an Ingress restricts nothing on a route
	none := ParseGatewaySettings("app", "r", map[string]string{
		p + "enable-global-auth": "true", p + "global-rate-limit": "0", p + "global-rate-limit-window": "1m", p + "enable-owasp-core-rules": "false",
	}, false)
	if len(none.Restricted) != 0 {
		t.Errorf("restricted %q", none.Restricted)
	}
	if rl := ParseGatewaySettings("app", "r", map[string]string{p + "global-rate-limit": "100"}, false); len(rl.Restricted) != 1 {
		t.Errorf("a global rate limit: restricted %q", rl.Restricted)
	}
}
