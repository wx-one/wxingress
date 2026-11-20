package gateway

import (
	"strings"
	"testing"

	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func TestTwoWeightedBackendsAreACanaryOfTheModule(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("b", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 3), backend("b", 80, 1)}})

	// without the module: splits
	if vs := onlyVS(t, build(t, testController(), b)); len(vs.Spec.Routes[0].Splits) != 2 {
		t.Errorf("without -dynamic-upstreams: %+v", vs.Spec.Routes[0])
	}

	c := testController()
	c.cfg.DynamicUpstreams = true
	vs := onlyVS(t, build(t, c, b))
	r := vs.Spec.Routes[0]
	if len(r.Splits) != 0 || r.Action == nil || r.Action.Proxy == nil {
		t.Fatalf("route %+v", r)
	}
	e := extrasOf(t, vs)
	a := actionAt(t, vs, e, "/")
	if a.Canary == nil || a.Canary.Primary != r.Action.Proxy.Upstream || a.Canary.Weight != 25 || a.Canary.WeightTotal != 100 || a.Canary.Upstream == a.Canary.Primary {
		t.Errorf("canary %+v", a.Canary)
	}
	if len(vs.Spec.Upstreams) != 2 {
		t.Errorf("upstreams %+v", vs.Spec.Upstreams)
	}
}

func TestCanaryByHeaderAndCookieOnARoute(t *testing.T) {
	t.Parallel()
	// weight 0: only the header or the cookie reaches the canary, as an
	// Ingress canary with canary-weight 0
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("b", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1), backend("b", 80, 0)}})
	b.s.HTTPRoutes[routeKey("r")].Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/canary-by-header": "X-Canary",
		"nginx.ingress.kubernetes.io/canary-by-cookie": "beta",
	}
	c := testController()
	c.cfg.DynamicUpstreams = true
	vs := onlyVS(t, build(t, c, b))
	a := actionAt(t, vs, extrasOf(t, vs), "/")
	if a.Canary == nil || a.Canary.Header != "X-Canary" || a.Canary.Cookie != "beta" || a.Canary.Weight != 0 || a.Canary.WeightTotal != 100 {
		t.Errorf("canary %+v", a.Canary)
	}

	// without the module the weight-0 backend gets nothing, as before
	vs = onlyVS(t, build(t, testController(), b))
	if r := vs.Spec.Routes[0]; r.Action == nil || len(r.Splits) != 0 {
		t.Errorf("without -dynamic-upstreams: %+v", r)
	}
}

func TestRouteLimitsAndSnippets(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("r", []string{"x.example.com"}, filterRoute("/r"))
	b.s.HTTPRoutes[routeKey("r")].Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/limit-connections":     "1",
		"nginx.ingress.kubernetes.io/limit-rate":            "8",
		"nginx.ingress.kubernetes.io/server-snippet":        "add_header X-Server on;",
		"nginx.ingress.kubernetes.io/configuration-snippet": "add_header X-Location on;",
	}
	c := testController()
	c.cfg.EnableSnippets = true
	vs := onlyVS(t, build(t, c, b))
	e := extrasOf(t, vs)
	a := actionAt(t, vs, e, "/r/")
	joined := strings.Join(a.Directives, " ")
	for _, want := range []string{"limit_conn ", "limit_conn_status 503;", "limit_rate 8k;", "add_header X-Location on;"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %q", want, a.Directives)
		}
	}
	if !strings.Contains(strings.Join(e.HTTP, "\n"), "limit_conn_zone ") {
		t.Errorf("no zone in %q", e.HTTP)
	}
	if len(e.Server) != 1 || e.Server[0] != "add_header X-Server on;" {
		t.Errorf("server %q", e.Server)
	}

	// without -enable-snippets the snippets go, the limits stay
	vs = onlyVS(t, build(t, testController(), b))
	e = extrasOf(t, vs)
	if len(e.Server) != 0 || strings.Contains(strings.Join(actionAt(t, vs, e, "/r/").Directives, " "), "X-Location") {
		t.Errorf("snippets without -enable-snippets: %q %q", e.Server, actionAt(t, vs, e, "/r/").Directives)
	}
}
