package k8s

import (
	"log/slog"
	"testing"

	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

func ingressOfClass(field, annotation string) *networking.Ingress {
	ing := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Name: "i", Namespace: "n", Annotations: map[string]string{}}}
	if field != "" {
		ing.Spec.IngressClassName = &field
	}
	if annotation != "" {
		ing.Annotations[ingressClassKey] = annotation
	}
	return ing
}

func TestIngressClassesByController(t *testing.T) {
	t.Parallel()
	store := cache.NewStore(cache.MetaNamespaceKeyFunc)
	for _, c := range []*networking.IngressClass{
		{ObjectMeta: meta_v1.ObjectMeta{Name: "nginx"}, Spec: networking.IngressClassSpec{Controller: "k8s.io/ingress-nginx"}},
		{ObjectMeta: meta_v1.ObjectMeta{Name: "wx"}, Spec: networking.IngressClassSpec{Controller: "nginx.wx1.eu/ingress-controller"}},
		{ObjectMeta: meta_v1.ObjectMeta{Name: "other"}, Spec: networking.IngressClassSpec{Controller: "example.com/other"}},
	} {
		_ = store.Add(c)
	}
	lbc := &LoadBalancerController{
		Logger:                  slog.New(slog.DiscardHandler),
		ingressClass:            "wx",
		ingressClassControllers: []string{"nginx.wx1.eu/ingress-controller", "k8s.io/ingress-nginx"},
		ingressClassLister:      store,
	}
	for _, tc := range []struct {
		what string
		ing  *networking.Ingress
		want bool
	}{
		{"our own class", ingressOfClass("wx", ""), true},
		{"ingress-nginx's class, its controller in the list", ingressOfClass("nginx", ""), true},
		{"by the annotation", ingressOfClass("", "nginx"), true},
		{"the annotation wins over the field", ingressOfClass("other", "nginx"), true},
		{"another controller's class", ingressOfClass("other", ""), false},
		{"a class nobody made", ingressOfClass("missing", ""), false},
		{"no class, no flag, no default", ingressOfClass("", ""), false},
	} {
		if got := lbc.HasCorrectIngressClass(tc.ing); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.what, got, tc.want)
		}
	}

	// without a class: with the flag, or when a served class is the default
	lbc.watchIngressWithoutClass = true
	if !lbc.HasCorrectIngressClass(ingressOfClass("", "")) {
		t.Error("-watch-ingress-without-class does not serve an Ingress without a class")
	}
	lbc.watchIngressWithoutClass = false
	_ = store.Update(&networking.IngressClass{
		ObjectMeta: meta_v1.ObjectMeta{Name: "nginx", Annotations: map[string]string{IngressClassDefaultAnnotation: "true"}},
		Spec:       networking.IngressClassSpec{Controller: "k8s.io/ingress-nginx"},
	})
	if !lbc.HasCorrectIngressClass(ingressOfClass("", "")) {
		t.Error("a served default IngressClass does not serve an Ingress without a class")
	}

	// a class of -ingress-class without any IngressClass is served by name
	bare := &LoadBalancerController{Logger: slog.New(slog.DiscardHandler), ingressClass: "wx", ingressClassControllers: []string{"nginx.wx1.eu/ingress-controller"}}
	if !bare.HasCorrectIngressClass(ingressOfClass("", "wx")) {
		t.Error("the -ingress-class name is not served without an IngressClass")
	}
}
