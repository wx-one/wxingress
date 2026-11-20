package gateway

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	"github.com/wx-one/wxingress/pkg/apis/configuration/validation"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/conditions"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

const (
	gwNs  = "infra"
	appNs = "app"
)

func ptr[T any](v T) *T { return &v }

func testController() *Controller {
	return &Controller{
		cfg: Config{
			ControllerName: DefaultControllerName,
			GatewayClass:   DefaultGatewayClass,
			IngressClass:   "nginx",
			HTTPPort:       80,
			HTTPSPort:      443,
			DefaultGateway: types.NamespacedName{Namespace: gwNs, Name: "gw"},
			Logger:         slog.New(slog.DiscardHandler),
		},
		logger:     logr.Discard(),
		previous:   make(map[string]*conf_v1.VirtualServer),
		validators: newValidators(),
	}
}

type stateBuilder struct {
	s graph.ClusterState
}

func newState() *stateBuilder {
	b := &stateBuilder{s: graph.ClusterState{
		GatewayClasses:  map[types.NamespacedName]*gatewayv1.GatewayClass{},
		Gateways:        map[types.NamespacedName]*gatewayv1.Gateway{},
		HTTPRoutes:      map[types.NamespacedName]*gatewayv1.HTTPRoute{},
		Services:        map[types.NamespacedName]*v1.Service{},
		Namespaces:      map[types.NamespacedName]*v1.Namespace{},
		ReferenceGrants: map[types.NamespacedName]*gatewayv1.ReferenceGrant{},
		Secrets:         map[types.NamespacedName]*v1.Secret{},
	}}
	b.s.GatewayClasses[types.NamespacedName{Name: DefaultGatewayClass}] = &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultGatewayClass, Generation: 1},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: DefaultControllerName},
	}
	for _, ns := range []string{gwNs, appNs} {
		b.s.Namespaces[types.NamespacedName{Name: ns}] = &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	}
	return b
}

func (b *stateBuilder) gateway(name string, listeners ...gatewayv1.Listener) *stateBuilder {
	b.s.Gateways[types.NamespacedName{Namespace: gwNs, Name: name}] = &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: gwNs, Name: name, Generation: 1},
		Spec: gatewayv1.GatewaySpec{
			GatewayClassName: DefaultGatewayClass,
			Listeners:        listeners,
		},
	}
	return b
}

func httpListener(name string, hostname string) gatewayv1.Listener {
	l := gatewayv1.Listener{
		Name:     gatewayv1.SectionName(name),
		Port:     80,
		Protocol: gatewayv1.HTTPProtocolType,
		AllowedRoutes: &gatewayv1.AllowedRoutes{
			Namespaces: &gatewayv1.RouteNamespaces{From: ptr(gatewayv1.NamespacesFromAll)},
		},
	}
	if hostname != "" {
		l.Hostname = ptr(gatewayv1.Hostname(hostname))
	}
	return l
}

func (b *stateBuilder) service(name string, port int32) *stateBuilder {
	b.s.Services[types.NamespacedName{Namespace: appNs, Name: name}] = &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name},
		Spec: v1.ServiceSpec{
			Ports: []v1.ServicePort{{Port: port, TargetPort: intstr.FromInt32(8080)}},
		},
	}
	return b
}

func (b *stateBuilder) route(name string, hostnames []string, rules ...gatewayv1.HTTPRouteRule) *stateBuilder {
	r := &gatewayv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name, Generation: 1},
		Spec: gatewayv1.HTTPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{
				ParentRefs: []gatewayv1.ParentReference{{
					Namespace: ptr(gatewayv1.Namespace(gwNs)),
					Name:      "gw",
				}},
			},
			Rules: rules,
		},
	}
	// what the API server defaults
	for i := range r.Spec.Rules {
		if len(r.Spec.Rules[i].Matches) == 0 {
			r.Spec.Rules[i].Matches = []gatewayv1.HTTPRouteMatch{{Path: prefix("/")}}
		}
	}
	for _, h := range hostnames {
		r.Spec.Hostnames = append(r.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	b.s.HTTPRoutes[types.NamespacedName{Namespace: appNs, Name: name}] = r
	return b
}

func backend(name string, port int32, weight int32) gatewayv1.HTTPBackendRef {
	return gatewayv1.HTTPBackendRef{BackendRef: gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{
			Name: gatewayv1.ObjectName(name),
			Port: ptr(gatewayv1.PortNumber(port)),
		},
		Weight: ptr(weight),
	}}
}

func prefix(p string) *gatewayv1.HTTPPathMatch {
	return &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchPathPrefix), Value: ptr(p)}
}

func build(t *testing.T, c *Controller, b *stateBuilder) Result {
	t.Helper()
	res := c.build(context.Background(), b.s)
	vsv := validation.NewVirtualServerValidator()
	for _, vs := range res.VirtualServers {
		if err := vsv.ValidateVirtualServer(ForValidation(vs)); err != nil {
			t.Errorf("VirtualServer %s for %s is invalid: %v", vs.Name, vs.Spec.Host, err)
		}
		if !IsGatewayVirtualServer(vs) {
			t.Errorf("VirtualServer %s is not marked as made from a Gateway", vs.Name)
		}
	}
	return res
}

func onlyVS(t *testing.T, res Result) *conf_v1.VirtualServer {
	t.Helper()
	if len(res.VirtualServers) != 1 {
		t.Fatalf("got %d VirtualServers, want 1 (skipped: %v)", len(res.VirtualServers), res.Skipped)
	}
	return res.VirtualServers[0]
}

func TestHostnamesHeaderMatchesAndWeights(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", httpListener("http", "")).
		service("a", 80).service("b", 8080).
		route("r", []string{"cafe.example.com"},
			gatewayv1.HTTPRouteRule{
				Matches: []gatewayv1.HTTPRouteMatch{{
					Path:    prefix("/coffee"),
					Headers: []gatewayv1.HTTPHeaderMatch{{Type: ptr(gatewayv1.HeaderMatchExact), Name: "X-Version", Value: "2"}},
				}},
				BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 80), backend("b", 8080, 20)},
			},
			gatewayv1.HTTPRouteRule{
				Matches:     []gatewayv1.HTTPRouteMatch{{Path: prefix("/coffee")}},
				BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)},
			},
		)
	vs := onlyVS(t, build(t, testController(), b))

	if vs.Spec.Host != "cafe.example.com" || vs.Namespace != gwNs || vs.Spec.IngressClass != "nginx" {
		t.Errorf("host %q namespace %q class %q", vs.Spec.Host, vs.Namespace, vs.Spec.IngressClass)
	}
	if vs.Annotations[AnnotationGateway] != gwNs+"/gw" {
		t.Errorf("gateway annotation %q", vs.Annotations[AnnotationGateway])
	}

	upstreams := map[string]string{}
	for _, u := range vs.Spec.Upstreams {
		upstreams[u.Service+":"+strconv.Itoa(int(u.Port))] = u.Name
	}
	ua, ub := upstreams["app/a:80"], upstreams["app/b:8080"]
	if ua == "" || ub == "" || len(upstreams) != 2 {
		t.Fatalf("upstreams %v", vs.Spec.Upstreams)
	}

	// /coffee is /coffee/ and = /coffee, both with the header match first
	// and the rule without conditions as their own action
	want := []conf_v1.Route{}
	for _, path := range []string{"/coffee/", "=/coffee"} {
		want = append(want, conf_v1.Route{
			Path: path,
			Matches: []conf_v1.Match{{
				Conditions: []conf_v1.Condition{{Header: "X-Version", Value: "2"}},
				Splits: []conf_v1.Split{
					{Weight: 80, Action: &conf_v1.Action{Pass: ua}},
					{Weight: 20, Action: &conf_v1.Action{Pass: ub}},
				},
			}},
			Action: &conf_v1.Action{Pass: ua},
		})
	}
	if d := cmp.Diff(want, plainRoutes(vs.Spec.Routes)); d != "" {
		t.Errorf("routes (-want +got):\n%s", d)
	}
}

func TestListenerWithoutHostnameIsCatchAll(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", httpListener("http", "")).
		service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	vs := onlyVS(t, build(t, testController(), b))
	if !IsCatchAllHost(vs.Spec.Host) {
		t.Errorf("host %q, want the catch-all", vs.Spec.Host)
	}
	if len(vs.Spec.Routes) != 1 || vs.Spec.Routes[0].Path != "/" || vs.Spec.Routes[0].Action == nil {
		t.Errorf("routes %+v", vs.Spec.Routes)
	}
}

func TestMissingBackendKeepsItsShareAs500(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", httpListener("http", "")).
		service("a", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{
			BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 50), backend("missing", 80, 50)},
		})
	vs := onlyVS(t, build(t, testController(), b))
	splits := vs.Spec.Routes[0].Splits
	if len(splits) != 2 || upstreamOf(splits[0].Action) == "" || splits[1].Action.Return == nil || splits[1].Action.Return.Code != 500 {
		t.Errorf("splits %+v", splits)
	}
}

func TestOnlyMatchesWithConditionsAnswer404Otherwise(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", httpListener("http", "")).
		service("a", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{
			Matches: []gatewayv1.HTTPRouteMatch{{
				Path:        &gatewayv1.HTTPPathMatch{Type: ptr(gatewayv1.PathMatchExact), Value: ptr("/exact")},
				Method:      ptr(gatewayv1.HTTPMethodPost),
				QueryParams: []gatewayv1.HTTPQueryParamMatch{{Type: ptr(gatewayv1.QueryParamMatchExact), Name: "v", Value: "1"}},
			}},
			BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)},
		})
	vs := onlyVS(t, build(t, testController(), b))
	r := vs.Spec.Routes[0]
	if r.Path != "=/exact" {
		t.Errorf("path %q", r.Path)
	}
	wantConds := []conf_v1.Condition{{Variable: "$request_method", Value: "POST"}, {Variable: "$args", Value: "~^(?:(?!v=)[^&]*&)*v=1(?:&|$)"}}
	if len(r.Matches) != 1 || !cmp.Equal(r.Matches[0].Conditions, wantConds) {
		t.Errorf("matches %+v", r.Matches)
	}
	if r.Action == nil || r.Action.Return == nil || r.Action.Return.Code != 404 {
		t.Errorf("default action %+v", r.Action)
	}
}

func TestOtherGatewayClassIsIgnored(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	b.s.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}].Spec.GatewayClassName = "other"
	if res := build(t, testController(), b); len(res.VirtualServers) != 0 {
		t.Errorf("got %d VirtualServers for a Gateway of another class", len(res.VirtualServers))
	}
}

func TestRouteStatusConditions(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", httpListener("http", "")).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("missing", 80, 1)}})
	res := build(t, testController(), b)
	var r *graph.L7Route
	for _, route := range res.Graph.Routes {
		r = route
	}
	if r == nil {
		t.Fatal("no route in the graph")
	}
	var resolved *conditions.Condition
	for i := range r.Conditions {
		if r.Conditions[i].Type == string(gatewayv1.RouteConditionResolvedRefs) {
			resolved = &r.Conditions[i]
		}
	}
	if resolved == nil || resolved.Status != metav1.ConditionFalse || resolved.Reason != string(gatewayv1.RouteReasonBackendNotFound) {
		t.Errorf("ResolvedRefs condition %+v", resolved)
	}
}

func TestGenerationOnlyChangesWithTheSpec(t *testing.T) {
	t.Parallel()
	c := testController()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("b", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	first := onlyVS(t, build(t, c, b))
	again := onlyVS(t, build(t, c, b))
	if again != first || again.Generation != 1 {
		t.Errorf("an unchanged build made a new VirtualServer (generation %d)", again.Generation)
	}
	b.route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("b", 80, 1)}})
	changed := onlyVS(t, build(t, c, b))
	if changed.Generation != 2 {
		t.Errorf("generation %d after a change, want 2", changed.Generation)
	}
}

func TestPercentages(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   []int64
		want []int
	}{
		{[]int64{1, 1}, []int{50, 50}},
		{[]int64{1, 1, 1}, []int{34, 33, 33}},
		{[]int64{80, 20}, []int{80, 20}},
		{[]int64{1, 999999}, []int{0, 100}},
		{[]int64{3, 3, 3, 1}, []int{30, 30, 30, 10}},
	} {
		if got := percentages(tc.in); !cmp.Equal(got, tc.want) {
			t.Errorf("percentages(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestSecretOfKeyPair(t *testing.T) {
	t.Parallel()
	got, ok := secretOfKeyPair("ssl_keypair_infra_cafe-secret")
	if !ok || got != (types.NamespacedName{Namespace: "infra", Name: "cafe-secret"}) {
		t.Errorf("got %v %v", got, ok)
	}
	if _, ok := secretOfKeyPair("other"); ok {
		t.Error("parsed an ID that is none")
	}
}

func TestNamesAreNoKubernetesNames(t *testing.T) {
	t.Parallel()
	n := virtualServerName(types.NamespacedName{Namespace: "a", Name: "gw.with.dots"}, "*.example.com", 8080)
	if !strings.HasPrefix(n, NamePrefix) || strings.ContainsAny(n, ".*") {
		t.Errorf("name %q", n)
	}
}

// upstreamOf is the upstream an action proxies to.
func upstreamOf(a *conf_v1.Action) string {
	if a == nil {
		return ""
	}
	if a.Proxy != nil {
		return a.Proxy.Upstream
	}
	return a.Pass
}

// plainRoutes are routes whose proxy actions - marked for their extras -
// are plain passes, to compare with.
func plainRoutes(routes []conf_v1.Route) []conf_v1.Route {
	plain := func(a *conf_v1.Action) *conf_v1.Action {
		if a == nil || a.Proxy == nil {
			return a
		}
		return &conf_v1.Action{Pass: a.Proxy.Upstream}
	}
	splits := func(in []conf_v1.Split) []conf_v1.Split {
		var out []conf_v1.Split
		for _, sp := range in {
			out = append(out, conf_v1.Split{Weight: sp.Weight, Action: plain(sp.Action)})
		}
		return out
	}
	out := make([]conf_v1.Route, 0, len(routes))
	for _, r := range routes {
		r.Action = plain(r.Action)
		r.Splits = splits(r.Splits)
		var matches []conf_v1.Match
		for _, m := range r.Matches {
			m.Action = plain(m.Action)
			m.Splits = splits(m.Splits)
			matches = append(matches, m)
		}
		r.Matches = matches
		out = append(out, r)
	}
	return out
}

func TestQueryParamRegex(t *testing.T) {
	t.Parallel()
	got := queryParamRegex(dataplane.HTTPQueryParamMatch{Name: "a.b", Value: "x+y", Type: dataplane.MatchTypeExact})
	if want := `^(?:(?!a\.b=)[^&]*&)*a\.b=x\+y(?:&|$)`; got != want {
		t.Errorf("%s, want %s", got, want)
	}
	got = queryParamRegex(dataplane.HTTPQueryParamMatch{Name: "v", Value: "[0-9]+", Type: dataplane.MatchTypeRegularExpression})
	if want := `^(?:(?!v=)[^&]*&)*v=[^&]*(?:[0-9]+)[^&]*(?:&|$)`; got != want {
		t.Errorf("%s, want %s", got, want)
	}
}

func routeKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: appNs, Name: name}
}
func serviceKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: appNs, Name: name}
}

func gwKey(name string) types.NamespacedName {
	return types.NamespacedName{Namespace: gwNs, Name: name}
}
