package gateway

import (
	"encoding/json"
	"testing"

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
