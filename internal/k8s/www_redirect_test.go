package k8s

import (
	"maps"
	"testing"
)

func TestWWWRedirects(t *testing.T) {
	t.Parallel()
	conf := createTestConfiguration()
	lbc := &LoadBalancerController{configuration: conf}

	site := createTestIngress("site", "example.com", "www.other.com")
	site.Annotations["nginx.ingress.kubernetes.io/from-to-www-redirect"] = "true"
	conf.AddOrUpdateIngress(site)
	valid := map[string]bool{"example.com": true, "www.other.com": true}

	if got, want := lbc.wwwRedirectsOf(site, valid), map[string]string{"example.com": "www.example.com", "www.other.com": "other.com"}; !maps.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	lbc.wwwRedirects = map[string]map[string]string{"Ingress/default/site": lbc.wwwRedirectsOf(site, valid)}
	if more := lbc.wwwRedirectChanges(nil); len(more) != 0 {
		t.Errorf("unchanged redirects reconfigure: %v", more)
	}

	// another Ingress takes www.example.com: no redirect from it, and the site is configured again
	conf.AddOrUpdateIngress(createTestIngress("www", "www.example.com"))
	if got, want := lbc.wwwRedirectsOf(site, valid), map[string]string{"www.other.com": "other.com"}; !maps.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if more := lbc.wwwRedirectChanges(nil); len(more) != 1 || more[0].Resource.GetKeyWithKind() != "Ingress/default/site" {
		t.Errorf("changes %v", more)
	}
}
