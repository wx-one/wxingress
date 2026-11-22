package configs

import (
	"testing"

	networking "k8s.io/api/networking/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Names of upstreams and config files are joined with an underscore, which no namespace, name, host, Service
// or port has, so two Ingresses never share them.
func TestNamesOfIngressesDoNotCollideAcrossNamespaces(t *testing.T) {
	t.Parallel()
	backend := func(svc string) *networking.IngressBackend {
		return &networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: svc, Port: networking.ServiceBackendPort{Number: 80}}}
	}
	a := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Namespace: "a-b", Name: "c"}}
	b := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Namespace: "a", Name: "b-c"}}
	if objectMetaToFileName(&a.ObjectMeta) == objectMetaToFileName(&b.ObjectMeta) {
		t.Errorf("both Ingresses have the config file %s", objectMetaToFileName(&a.ObjectMeta))
	}
	if keyToFileName("a-b/c") != objectMetaToFileName(&a.ObjectMeta) {
		t.Errorf("the file of the key %s is not that of the Ingress %s", keyToFileName("a-b/c"), objectMetaToFileName(&a.ObjectMeta))
	}
	if getNameForUpstream(a, "x.example.com", backend("svc")) == getNameForUpstream(b, "x.example.com", backend("svc")) {
		t.Errorf("both Ingresses have the upstream %s", getNameForUpstream(a, "x.example.com", backend("svc")))
	}
	// the name of the Ingress against the host, and the host against the Service
	c := &networking.Ingress{ObjectMeta: meta_v1.ObjectMeta{Namespace: "a", Name: "b"}}
	if getNameForUpstream(c, "x-svc", backend("80")) == getNameForUpstream(c, "x", backend("svc-80")) {
		t.Errorf("host and Service run into each other")
	}
	if getNameForRedirectLocation(a) == getNameForRedirectLocation(b) {
		t.Errorf("both Ingresses have the login location %s", getNameForRedirectLocation(a))
	}
}

// A server alias is given once, to the first server of the Ingress, and never as a host of the Ingress itself.
func TestServerAliasOnceAndNotAnOwnHost(t *testing.T) {
	t.Parallel()
	ingEx := createCafeIngressEx()
	rule := ingEx.Ingress.Spec.Rules[0]
	rule.Host = "shop.example.com"
	ingEx.Ingress.Spec.Rules = append(ingEx.Ingress.Spec.Rules, rule)
	ingEx.ValidHosts["shop.example.com"] = true
	ingEx.Ingress.Annotations[IngressNginxServerAliasAnnotation] = "www.cafe.example.com,Shop.example.com"

	cfg, _ := generateCanaryCfg(t, &ingEx, false)
	if len(cfg.Servers) != 2 {
		t.Fatalf("servers: %d", len(cfg.Servers))
	}
	if got := cfg.Servers[0].ServerAliases; len(got) != 1 || got[0] != "www.cafe.example.com" {
		t.Errorf("aliases of the first server %v, want www.cafe.example.com", got)
	}
	if got := cfg.Servers[1].ServerAliases; len(got) != 0 {
		t.Errorf("aliases of the second server %v, want none", got)
	}
}
