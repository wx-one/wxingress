package gateway

import (
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	"github.com/wx-one/wxingress/internal/gateway/extras"
)

// extrasOf decodes the extras of vs and checks that every action key its
// routes name has extras and every one of them is named.
func extrasOf(t *testing.T, vs *conf_v1.VirtualServer) *extras.Extras {
	t.Helper()
	e, err := extras.Decode(vs.Annotations[extras.Annotation])
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	mark := func(a *conf_v1.Action) {
		switch {
		case a == nil:
		case a.Proxy != nil:
			for _, h := range a.Proxy.RequestHeaders.Set {
				if h.Name == extras.MarkerHeader {
					named[h.Value] = true
				}
			}
		case a.Redirect != nil:
			key, ok := strings.CutPrefix(a.Redirect.URL, "http://")
			key, ok2 := strings.CutSuffix(key, ".action.wxingress.invalid")
			if !ok || !ok2 {
				t.Errorf("redirect %q has no mark", a.Redirect.URL)
			}
			named[key] = true
		}
	}
	for _, r := range vs.Spec.Routes {
		mark(r.Action)
		for _, s := range r.Splits {
			mark(s.Action)
		}
		for _, m := range r.Matches {
			mark(m.Action)
			for _, s := range m.Splits {
				mark(s.Action)
			}
		}
	}
	for k := range named {
		if e.Actions[k] == nil {
			t.Errorf("action %s has no extras", k)
		}
	}
	for k := range e.Actions {
		if !named[k] {
			t.Errorf("extras %s belong to no action", k)
		}
	}
	return e
}

// actionAt is the extras of the action of the route with path.
func actionAt(t *testing.T, vs *conf_v1.VirtualServer, e *extras.Extras, path string) *extras.Action {
	t.Helper()
	for _, r := range vs.Spec.Routes {
		if r.Path != path {
			continue
		}
		switch {
		case r.Action != nil && r.Action.Proxy != nil:
			return e.Actions[r.Action.Proxy.RequestHeaders.Set[0].Value]
		case r.Action != nil && r.Action.Redirect != nil:
			key := strings.TrimSuffix(strings.TrimPrefix(r.Action.Redirect.URL, "http://"), ".action.wxingress.invalid")
			return e.Actions[key]
		}
	}
	t.Fatalf("no action at %s in %+v", path, vs.Spec.Routes)
	return nil
}

func filterRoute(path string, filters ...gatewayv1.HTTPRouteFilter) gatewayv1.HTTPRouteRule {
	return gatewayv1.HTTPRouteRule{
		Matches:     []gatewayv1.HTTPRouteMatch{{Path: prefix(path)}},
		Filters:     filters,
		BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)},
	}
}

func TestRequestAndResponseHeaderModifiers(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("r", []string{"x.example.com"},
			filterRoute("/req", gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterRequestHeaderModifier,
				RequestHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set:    []gatewayv1.HTTPHeader{{Name: "X-Set", Value: "s"}},
					Add:    []gatewayv1.HTTPHeader{{Name: "X-Add", Value: "a"}},
					Remove: []string{"X-Remove"},
				},
			}),
			filterRoute("/resp", gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterResponseHeaderModifier,
				ResponseHeaderModifier: &gatewayv1.HTTPHeaderFilter{
					Set:    []gatewayv1.HTTPHeader{{Name: "X-Set", Value: "s"}},
					Add:    []gatewayv1.HTTPHeader{{Name: "X-Add", Value: "a"}},
					Remove: []string{"X-Remove"},
				},
			}),
		)
	vs := onlyVS(t, build(t, testController(), b))
	e := extrasOf(t, vs)

	req := actionAt(t, vs, e, "/req/")
	host := "${" + newExtrasBuilder(vs.Name).prefix + "_host}"
	addVar := newExtrasBuilder(vs.Name).variable("add", "X-Add")
	want := []extras.Header{
		{Name: "Host", Value: host},
		{Name: "X-Set", Value: "s"},
		{Name: "X-Add", Value: "${" + addVar + "}a"},
		{Name: "X-Remove", Value: ""},
	}
	if d := cmp.Diff(want, req.SetHeaders); d != "" {
		t.Errorf("request headers (-want +got):\n%s", d)
	}
	http := strings.Join(e.HTTP, "\n")
	for _, s := range []string{"map $http_host $", "map $http_x_add $" + addVar, `default "$http_x_add,";`} {
		if !strings.Contains(http, s) {
			t.Errorf("http context has no %q:\n%s", s, http)
		}
	}

	resp := actionAt(t, vs, e, "/resp/")
	if !cmp.Equal(resp.HideHeaders, []string{"X-Set", "X-Remove"}) {
		t.Errorf("hidden %v", resp.HideHeaders)
	}
	if !cmp.Equal(resp.AddHeaders, []extras.Header{{Name: "X-Set", Value: "s"}, {Name: "X-Add", Value: "a"}}) {
		t.Errorf("added %v", resp.AddHeaders)
	}
}

func redirectFilter(f gatewayv1.HTTPRequestRedirectFilter) gatewayv1.HTTPRouteFilter {
	return gatewayv1.HTTPRouteFilter{Type: gatewayv1.HTTPRouteFilterRequestRedirect, RequestRedirect: &f}
}

func TestRedirects(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", listenerOn("http", 8080, "")).service("a", 80).
		route("r", []string{"x.example.com"},
			filterRoute("/host", redirectFilter(gatewayv1.HTTPRequestRedirectFilter{
				Hostname: ptr(gatewayv1.PreciseHostname("example.org")), StatusCode: ptr(301),
			})),
			filterRoute("/scheme", redirectFilter(gatewayv1.HTTPRequestRedirectFilter{Scheme: ptr("https")})),
			filterRoute("/port", redirectFilter(gatewayv1.HTTPRequestRedirectFilter{Port: ptr(gatewayv1.PortNumber(8443)), StatusCode: ptr(303)})),
			filterRoute("/prefix", redirectFilter(gatewayv1.HTTPRequestRedirectFilter{
				Path: &gatewayv1.HTTPPathModifier{Type: gatewayv1.PrefixMatchHTTPPathModifier, ReplacePrefixMatch: ptr("/new")},
			})),
		)
	o := newOwnAddressController(t)
	res := build(t, o.Controller, b)
	vs := onlyVS(t, res)
	e := extrasOf(t, vs)

	for path, want := range map[string]string{
		"/host/":   "301 $scheme://example.org:8080$request_uri",
		"/scheme/": "302 https://$host$request_uri",
		"/port/":   "303 $scheme://$host:8443$request_uri",
	} {
		a := actionAt(t, vs, e, path)
		if a.Return == nil {
			t.Errorf("%s: no return", path)
			continue
		}
		if got := strings.Join([]string{itoa(a.Return.Code), a.Return.Text}, " "); got != want {
			t.Errorf("%s: %q, want %q", path, got, want)
		}
	}

	p := actionAt(t, vs, e, "/prefix/")
	if p.Return == nil || !strings.HasPrefix(p.Return.Text, "$scheme://$host:8080$wxgw_") || !strings.HasSuffix(p.Return.Text, "$is_args$args") {
		t.Errorf("prefix redirect %+v", p.Return)
	}
	if http := strings.Join(e.HTTP, "\n"); !strings.Contains(http, `"~^/prefix(?:/([^?]*))?" "/new$1"`) && !strings.Contains(http, `"~^/prefix([^?]*)?" "/new$1"`) {
		t.Errorf("no map for the prefix:\n%s", http)
	}
}

func TestURLRewrite(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("r", []string{"x.example.com"},
			filterRoute("/p", gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterURLRewrite,
				URLRewrite: &gatewayv1.HTTPURLRewriteFilter{
					Hostname: ptr(gatewayv1.PreciseHostname("internal.example.org")),
					Path:     &gatewayv1.HTTPPathModifier{Type: gatewayv1.FullPathHTTPPathModifier, ReplaceFullPath: ptr("/full")},
				},
			}),
		)
	vs := onlyVS(t, build(t, testController(), b))
	e := extrasOf(t, vs)
	a := actionAt(t, vs, e, "/p/")
	if a.SetHeaders[0] != (extras.Header{Name: "Host", Value: "internal.example.org"}) {
		t.Errorf("host %v", a.SetHeaders)
	}
	if len(a.Rewrites) != 1 || !strings.HasPrefix(a.Rewrites[0], "^ $wxgw_") || !strings.HasSuffix(a.Rewrites[0], " break") {
		t.Errorf("rewrites %v", a.Rewrites)
	}
	if http := strings.Join(e.HTTP, "\n"); !strings.Contains(http, `"~^" "/full";`) {
		t.Errorf("no map for the full path:\n%s", http)
	}
}

func TestRequestMirror(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("m", 80).
		route("r", []string{"x.example.com"},
			filterRoute("/", gatewayv1.HTTPRouteFilter{
				Type: gatewayv1.HTTPRouteFilterRequestMirror,
				RequestMirror: &gatewayv1.HTTPRequestMirrorFilter{
					BackendRef: gatewayv1.BackendObjectReference{Name: "m", Port: ptr(gatewayv1.PortNumber(80))},
					Percent:    ptr(int32(25)),
				},
			}),
		)
	vs := onlyVS(t, build(t, testController(), b))
	e := extrasOf(t, vs)
	main := actionAt(t, vs, e, "/")
	if len(main.Directives) != 1 || !strings.HasPrefix(main.Directives[0], `mirror "/_ngf-internal-mirror`) {
		t.Fatalf("directives %v", main.Directives)
	}
	target := strings.TrimSuffix(strings.TrimPrefix(main.Directives[0], `mirror "`), `";`)
	m := actionAt(t, vs, e, "="+target)
	if len(m.Directives) != 2 || m.Directives[0] != "internal;" || !strings.Contains(m.Directives[1], "return 204") {
		t.Errorf("the mirror's target %v", m.Directives)
	}
	if !cmp.Equal(m.Rewrites, []string{"^ $request_uri_no_args break"}) {
		t.Errorf("the mirror's rewrites %v", m.Rewrites)
	}
	if http := strings.Join(e.HTTP, "\n"); !strings.Contains(http, "split_clients $request_id") || !strings.Contains(http, `25.00% "1";`) {
		t.Errorf("no sample of the mirror:\n%s", http)
	}
	if len(vs.Spec.Upstreams) != 2 {
		t.Errorf("upstreams %v, want the backend and the mirror's", vs.Spec.Upstreams)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestH2CBackends(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("h", 80).
		route("r", []string{"x.example.com"}, filterRoute("/h2c"), filterRoute("/plain"))
	b.s.HTTPRoutes[routeKey("r")].Spec.Rules[0].BackendRefs = []gatewayv1.HTTPBackendRef{backend("h", 80, 1)}
	b.s.Services[serviceKey("h")].Spec.Ports[0].AppProtocol = ptr("kubernetes.io/h2c")
	vs := onlyVS(t, build(t, testController(), b))
	e := extrasOf(t, vs)
	if v := actionAt(t, vs, e, "/h2c/").HTTPVersion; v != "2" {
		t.Errorf("h2c backend: HTTP version %q", v)
	}
	if v := actionAt(t, vs, e, "/plain/").HTTPVersion; v != "" {
		t.Errorf("plain backend: HTTP version %q", v)
	}
}
