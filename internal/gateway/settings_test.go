package gateway

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	"github.com/wx-one/wxingress/internal/gateway/extras"
	"github.com/wx-one/wxingress/internal/nginx"
)

func TestRouteSettingsAndPolicies(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).service("b", 80).
		route("tuned", []string{"x.example.com"}, filterRoute("/tuned")).
		route("plain", []string{"x.example.com"}, filterRoute("/plain"))
	tuned := b.s.HTTPRoutes[routeKey("tuned")]
	tuned.Annotations = map[string]string{
		"nginx.ingress.kubernetes.io/proxy-read-timeout": "120",
		"nginx.ingress.kubernetes.io/proxy-body-size":    "8m",
		"nginx.ingress.kubernetes.io/load-balance":       "ewma",
		AnnotationPolicies: "rate, other/auth",
	}
	gw := b.s.Gateways[gwKey("gw")]
	gw.Annotations = map[string]string{AnnotationPolicies: "allow"}
	// the Policy of another namespace with that namespace's consent
	b.policyGrant("other", "HTTPRoute", appNs, "auth")

	vs := onlyVS(t, build(t, testController(), b))
	if want := []conf_v1.PolicyReference{{Name: "allow", Namespace: gwNs}}; len(vs.Spec.Policies) != 1 || vs.Spec.Policies[0] != want[0] {
		t.Errorf("the Gateway's policies %+v", vs.Spec.Policies)
	}
	byPath := map[string]conf_v1.Route{}
	for _, r := range vs.Spec.Routes {
		byPath[r.Path] = r
	}
	if p := byPath["/tuned/"].Policies; len(p) != 2 || p[0] != (conf_v1.PolicyReference{Name: "rate", Namespace: appNs}) ||
		p[1] != (conf_v1.PolicyReference{Name: "auth", Namespace: "other"}) {
		t.Errorf("the route's policies %+v", p)
	}
	if p := byPath["/plain/"].Policies; len(p) != 0 {
		t.Errorf("another route got policies %+v", p)
	}

	ups := map[string]conf_v1.Upstream{}
	for _, u := range vs.Spec.Upstreams {
		ups[u.Name] = u
	}
	tunedUp := ups[upstreamOf(byPath["/tuned/"].Action)]
	plainUp := ups[upstreamOf(byPath["/plain/"].Action)]
	if tunedUp.ProxyReadTimeout != "120s" || tunedUp.ClientMaxBodySize != "8m" {
		t.Errorf("the route's upstream %+v", tunedUp)
	}
	if plainUp.Name == tunedUp.Name || plainUp.ProxyReadTimeout != "" {
		t.Errorf("the other route shares the settings: %+v", plainUp)
	}

	e := extrasOf(t, vs)
	raw, ok := e.Balancers[tunedUp.Name]
	if !ok || len(e.Balancers) != 1 {
		t.Fatalf("balancers %v", e.Balancers)
	}
	var rule nginx.Balancer
	if err := json.Unmarshal(raw, &rule); err != nil || rule.Algorithm != "ewma" || rule.Backend != appNs+"-a-80" {
		t.Errorf("rule %+v %v", rule, err)
	}
	_ = extras.Annotation
	_ = gatewayv1.HTTPRouteRule{}
}

// policyGrant adds a ReferenceGrant in ns that lets objects of kind in from
// name the Policy name ("" for all of ns).
func (b *stateBuilder) policyGrant(ns, kind, from, name string) *stateBuilder {
	to := gatewayv1.ReferenceGrantTo{Group: "k8s.nginx.org", Kind: "Policy"}
	if name != "" {
		to.Name = ptr(gatewayv1.ObjectName(name))
	}
	rg := &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "grant-" + kind + "-" + from + "-" + name},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: gatewayv1.Kind(kind), Namespace: gatewayv1.Namespace(from)}},
			To:   []gatewayv1.ReferenceGrantTo{to},
		},
	}
	b.s.ReferenceGrants[types.NamespacedName{Namespace: ns, Name: rg.Name}] = rg
	return b
}

// TestPoliciesOfAnotherNamespaceNeedAReferenceGrant: a route or Gateway
// names a Policy of its own namespace freely, one of another only with a
// ReferenceGrant there for its kind and namespace; without one the Policy is
// not applied and what named it answers 500.
func TestPoliciesOfAnotherNamespaceNeedAReferenceGrant(t *testing.T) {
	t.Parallel()
	newB := func() *stateBuilder {
		b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
			route("thief", []string{"x.example.com"}, filterRoute("/thief")).
			route("plain", []string{"x.example.com"}, filterRoute("/plain"))
		b.s.HTTPRoutes[routeKey("thief")].Annotations = map[string]string{AnnotationPolicies: "own, victim/secret-jwt"}
		return b
	}
	routesOf := func(vs *conf_v1.VirtualServer) map[string]conf_v1.Route {
		out := map[string]conf_v1.Route{}
		for _, r := range vs.Spec.Routes {
			out[r.Path] = r
		}
		return out
	}

	for name, grant := range map[string]func(*stateBuilder){
		"no grant":                 func(*stateBuilder) {},
		"grant for another kind":   func(b *stateBuilder) { b.policyGrant("victim", "GRPCRoute", appNs, "") },
		"grant for another ns":     func(b *stateBuilder) { b.policyGrant("victim", "HTTPRoute", gwNs, "") },
		"grant for another name":   func(b *stateBuilder) { b.policyGrant("victim", "HTTPRoute", appNs, "other") },
		"grant in the wrong place": func(b *stateBuilder) { b.policyGrant(appNs, "HTTPRoute", appNs, "secret-jwt") },
	} {
		b := newB()
		grant(b)
		res := build(t, testController(), b)
		r := routesOf(onlyVS(t, res))
		thief := r["/thief/"]
		for _, p := range thief.Policies {
			if p.Namespace == "victim" {
				t.Errorf("%s: the Policy of another namespace is applied: %+v", name, thief.Policies)
			}
		}
		if thief.Action == nil || thief.Action.Return == nil || thief.Action.Return.Code != 500 {
			t.Errorf("%s: the route that named it does not answer 500: %+v", name, thief.Action)
		}
		if plain := r["/plain/"]; plain.Action == nil || plain.Action.Proxy == nil {
			t.Errorf("%s: another route is affected: %+v", name, plain.Action)
		}
	}

	// with the grant, by name or for the namespace
	for _, name := range []string{"secret-jwt", ""} {
		b := newB()
		b.policyGrant("victim", "HTTPRoute", appNs, name)
		r := routesOf(onlyVS(t, build(t, testController(), b)))
		thief := r["/thief/"]
		if len(thief.Policies) != 2 || thief.Policies[1] != (conf_v1.PolicyReference{Namespace: "victim", Name: "secret-jwt"}) {
			t.Errorf("grant %q: policies %+v", name, thief.Policies)
		}
		if thief.Action == nil || thief.Action.Proxy == nil {
			t.Errorf("grant %q: action %+v", name, thief.Action)
		}
	}

	// the Gateway as well: every request of its servers answers 500
	b := newB()
	b.s.HTTPRoutes[routeKey("thief")].Annotations = nil
	b.s.Gateways[gwKey("gw")].Annotations = map[string]string{AnnotationPolicies: "victim/secret-jwt"}
	vs := onlyVS(t, build(t, testController(), b))
	if len(vs.Spec.Policies) != 0 || len(vs.Spec.Routes) != 1 || vs.Spec.Routes[0].Action.Return == nil || vs.Spec.Routes[0].Action.Return.Code != 500 {
		t.Errorf("the Gateway's denied Policy: policies %+v routes %+v", vs.Spec.Policies, vs.Spec.Routes)
	}
	b.policyGrant("victim", "Gateway", gwNs, "secret-jwt")
	vs = onlyVS(t, build(t, testController(), b))
	if len(vs.Spec.Policies) != 1 || len(vs.Spec.Routes) < 2 {
		t.Errorf("the Gateway's granted Policy: policies %+v routes %+v", vs.Spec.Policies, vs.Spec.Routes)
	}
}

// TestModuleBalancedRoutesKeepNginxMethod: with the wxingress module a
// route's load-balance is a rule of the module, and the upstream keeps
// nginx's method - switching between ewma and round_robin changes no
// VirtualServer, which would reload nginx. Without the module ewma is
// least_time, as for an Ingress.
func TestModuleBalancedRoutesKeepNginxMethod(t *testing.T) {
	t.Parallel()
	upstreamWith := func(dynamic bool, method string) conf_v1.Upstream {
		b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
			route("tuned", []string{"x.example.com"}, filterRoute("/tuned"))
		b.s.HTTPRoutes[routeKey("tuned")].Annotations = map[string]string{"nginx.ingress.kubernetes.io/load-balance": method}
		c := testController()
		c.cfg.DynamicUpstreams = dynamic
		vs := onlyVS(t, build(t, c, b))
		if len(vs.Spec.Upstreams) != 1 {
			t.Fatalf("upstreams %+v", vs.Spec.Upstreams)
		}
		return vs.Spec.Upstreams[0]
	}
	if u := upstreamWith(true, "ewma"); u.LBMethod != "" {
		t.Errorf("with the module ewma is %q on the upstream, want nginx's method", u.LBMethod)
	}
	if a, b := upstreamWith(true, "ewma"), upstreamWith(true, "round_robin"); !cmp.Equal(a, b) {
		t.Errorf("ewma and round_robin differ in the upstream: %s", cmp.Diff(a, b))
	}
	if u := upstreamWith(false, "ewma"); u.LBMethod != "least_time last_byte" {
		t.Errorf("without the module ewma is %q, want least_time last_byte", u.LBMethod)
	}
}
