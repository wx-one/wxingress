package k8s

import (
	"strings"
	"testing"

	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func backendOf(name string) networking.IngressBackend {
	return networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: name, Port: networking.ServiceBackendPort{Number: 80}}}
}

func TestAsIngressNginx(t *testing.T) {
	t.Parallel()
	prefix := networking.PathTypePrefix
	rule := func(host, path, svc string) networking.IngressRule {
		return networking.IngressRule{Host: host, IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{
			Paths: []networking.HTTPIngressPath{{Path: path, PathType: &prefix, Backend: backendOf(svc)}},
		}}}
	}
	twice := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Name: "i", Namespace: "n"},
		Spec: networking.IngressSpec{Rules: []networking.IngressRule{rule("a.test", "/x", "x"), rule("b.test", "/", "b"), rule("a.test", "/y", "y")}}}
	got := asIngressNginx(twice, false)
	if len(got.Spec.Rules) != 2 || len(got.Spec.Rules[0].HTTP.Paths) != 2 || got.Spec.Rules[0].HTTP.Paths[1].Path != "/y" {
		t.Errorf("merged %+v", got.Spec.Rules)
	}
	if len(twice.Spec.Rules) != 3 {
		t.Error("the Ingress of the informer was changed")
	}
	if err := validateIngressSpec(&got.Spec, nil, false).ToAggregate(); err != nil {
		t.Errorf("the merged Ingress is invalid: %v", err)
	}

	only := &networking.Ingress{Spec: networking.IngressSpec{DefaultBackend: &networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: "d", Port: networking.ServiceBackendPort{Number: 80}}}}}
	if got := asIngressNginx(only, false); len(got.Spec.Rules) != 0 {
		t.Error("a catch-all without -allow-empty-ingress-host")
	}
	got = asIngressNginx(only, true)
	if len(got.Spec.Rules) != 1 || got.Spec.Rules[0].Host != "" || got.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name != "d" {
		t.Errorf("catch-all %+v", got.Spec.Rules)
	}
	if err := validateIngressSpec(&got.Spec, nil, true).ToAggregate(); err != nil {
		t.Errorf("the catch-all is invalid: %v", err)
	}
	plain := &networking.Ingress{Spec: networking.IngressSpec{Rules: []networking.IngressRule{rule("a.test", "/", "a")}}}
	if asIngressNginx(plain, true) != plain {
		t.Error("an Ingress that needs nothing was copied")
	}
}

// A host listed twice with the same path, or a path twice in a rule, is one
// location of the first backend, as ingress-nginx serves it - two "location /"
// in a server are a configuration nginx refuses, for every host of the
// controller; an Exact path and a Prefix one of the same path stay both.
func TestAsIngressNginxKeepsTheFirstOfAPath(t *testing.T) {
	t.Parallel()
	impl, exact := networking.PathTypeImplementationSpecific, networking.PathTypeExact
	path := func(p string, pt *networking.PathType, svc string) networking.HTTPIngressPath {
		return networking.HTTPIngressPath{Path: p, PathType: pt, Backend: backendOf(svc)}
	}
	rule := func(host string, paths ...networking.HTTPIngressPath) networking.IngressRule {
		return networking.IngressRule{Host: host, IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{Paths: paths}}}
	}
	ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Name: "frontend", Namespace: "n"}, Spec: networking.IngressSpec{Rules: []networking.IngressRule{
		rule("glue.test", path("/", &impl, "first")),
		rule("www.test", path("/", &impl, "www")),
		rule("glue.test", path("/", &impl, "second")),
		rule("api.test", path("/a", &impl, "a1"), path("/a", &impl, "a2"), path("/a", &exact, "exact")),
	}}}
	got := asIngressNginx(ing, false)
	want := map[string][]string{"glue.test": {"/ first"}, "www.test": {"/ www"}, "api.test": {"/a a1", "/a exact"}}
	if len(got.Spec.Rules) != len(want) {
		t.Fatalf("rules %+v", got.Spec.Rules)
	}
	for _, r := range got.Spec.Rules {
		var paths []string
		for _, p := range r.HTTP.Paths {
			paths = append(paths, p.Path+" "+p.Backend.Service.Name)
		}
		if strings.Join(paths, ",") != strings.Join(want[r.Host], ",") {
			t.Errorf("%s: %v, want %v", r.Host, paths, want[r.Host])
		}
	}
	if err := validateIngressSpec(&got.Spec, nil, false).ToAggregate(); err != nil {
		t.Errorf("the Ingress is invalid: %v", err)
	}
	if len(ing.Spec.Rules) != 4 || len(ing.Spec.Rules[3].HTTP.Paths) != 3 {
		t.Error("the Ingress of the informer was changed")
	}
}
