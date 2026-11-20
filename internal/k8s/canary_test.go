package k8s

import (
	"strings"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	nl "github.com/wx-one/wxingress/internal/logger"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func createTestIngressWithPath(name string, host string, path string, service string, created time.Time) *networking.Ingress {
	prefix := networking.PathTypePrefix
	return &networking.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(created),
			Annotations:       map[string]string{"kubernetes.io/ingress.class": "nginx"},
		},
		Spec: networking.IngressSpec{
			Rules: []networking.IngressRule{{
				Host: host,
				IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{
					Paths: []networking.HTTPIngressPath{{
						Path:     path,
						PathType: &prefix,
						Backend: networking.IngressBackend{Service: &networking.IngressServiceBackend{
							Name: service,
							Port: networking.ServiceBackendPort{Number: 80},
						}},
					}},
				}},
			}},
		},
	}
}

func createTestCanaryIngress(name string, host string, path string, service string, created time.Time, annotations map[string]string) *networking.Ingress {
	ing := createTestIngressWithPath(name, host, path, service, created)
	ing.Annotations[configs.CanaryAnnotation] = "true"
	for k, v := range annotations {
		ing.Annotations[k] = v
	}
	return ing
}

func createTestCanaryConfiguration() *Configuration {
	c := createTestConfiguration()
	c.isDynamicUpstreams = true
	return c
}

func canariesOfChange(t *testing.T, changes []ResourceChange, key string) []*networking.Ingress {
	t.Helper()
	for _, change := range changes {
		if change.Resource.GetKeyWithKind() == key {
			if change.Op != AddOrUpdate {
				t.Fatalf("change of %s is %v, want AddOrUpdate", key, change.Op)
			}
			return change.Resource.(*IngressConfiguration).Canaries
		}
	}
	t.Fatalf("no change of %s in %+v", key, changes)
	return nil
}

func TestCanaryIngressTakesNoHost(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	now := time.Now()

	primary := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now)
	c.AddOrUpdateIngress(primary)

	// a canary older than the primary would win the host if it took one
	canary := createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now.Add(-time.Hour), map[string]string{configs.CanaryWeightAnnotation: "10"})
	changes, problems := c.AddOrUpdateIngress(canary)

	if len(problems) != 0 {
		t.Errorf("problems %+v, want none", problems)
	}
	canaries := canariesOfChange(t, changes, "Ingress/default/cafe")
	if len(canaries) != 1 || canaries[0] != canary {
		t.Errorf("canaries of the primary %v, want the canary", canaries)
	}
	if len(changes) != 1 {
		t.Errorf("changes %+v, want only the primary", changes)
	}

	resources := c.GetResources()
	if len(resources) != 1 || resources[0].GetKeyWithKind() != "Ingress/default/cafe" {
		t.Errorf("resources %+v, want only the primary", resources)
	}
	ingConfig := resources[0].(*IngressConfiguration)
	if !ingConfig.ValidHosts["cafe.example.com"] || len(ingConfig.Warnings) != 0 {
		t.Errorf("the primary has valid hosts %v and warnings %v", ingConfig.ValidHosts, ingConfig.Warnings)
	}

	if found := c.FindResourcesForService("default", "tea-canary-svc"); len(found) != 1 || found[0] != resources[0] {
		t.Errorf("resources for the service of the canary %+v, want the primary", found)
	}
	if found := c.FindResourcesForEndpoints("default", "tea-canary-svc"); len(found) != 1 {
		t.Errorf("resources for the endpoints of the canary %+v, want the primary", found)
	}

	// a new weight changes the primary
	updated := canary.DeepCopy()
	updated.Annotations[configs.CanaryWeightAnnotation] = "50"
	changes, _ = c.AddOrUpdateIngress(updated)
	if canaries := canariesOfChange(t, changes, "Ingress/default/cafe"); len(canaries) != 1 || canaries[0] != updated {
		t.Errorf("canaries after the update %v, want the updated canary", canaries)
	}

	// and so does its deletion
	changes, _ = c.DeleteIngress("default/cafe-canary")
	if canaries := canariesOfChange(t, changes, "Ingress/default/cafe"); len(canaries) != 0 {
		t.Errorf("canaries after the deletion %v, want none", canaries)
	}
}

func TestCanaryIngressOrder(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	now := time.Now()

	c.AddOrUpdateIngress(createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now))
	younger := createTestCanaryIngress("a-canary", "cafe.example.com", "/tea", "a-svc", now.Add(time.Minute), nil)
	older := createTestCanaryIngress("b-canary", "cafe.example.com", "/tea", "b-svc", now, nil)
	c.AddOrUpdateIngress(younger)
	changes, _ := c.AddOrUpdateIngress(older)

	canaries := canariesOfChange(t, changes, "Ingress/default/cafe")
	if len(canaries) != 2 || canaries[0] != older || canaries[1] != younger {
		t.Errorf("canaries %v, want the older first", canaries)
	}
}

func TestCanaryIngressProblems(t *testing.T) {
	t.Parallel()
	now := time.Now()
	tests := []struct {
		name               string
		dynamicUpstreams   bool
		canary             *networking.Ingress
		wantMessagePrefix  string
		wantPrimaryChanges bool
	}{
		{
			name:              "without dynamic upstreams",
			dynamicUpstreams:  false,
			canary:            createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now, nil),
			wantMessagePrefix: "The canary is ignored: canary Ingresses need -dynamic-upstreams",
		},
		{
			name:              "invalid weight",
			dynamicUpstreams:  true,
			canary:            createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now, map[string]string{configs.CanaryWeightAnnotation: "lots"}),
			wantMessagePrefix: "The canary is ignored: " + configs.CanaryWeightAnnotation,
		},
		{
			name:              "another path",
			dynamicUpstreams:  true,
			canary:            createTestCanaryIngress("cafe-canary", "cafe.example.com", "/coffee", "coffee-canary-svc", now, nil),
			wantMessagePrefix: "The canary has no Ingress",
		},
		{
			name:              "another host",
			dynamicUpstreams:  true,
			canary:            createTestCanaryIngress("cafe-canary", "tea.example.com", "/tea", "tea-canary-svc", now, nil),
			wantMessagePrefix: "The canary has no Ingress",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c := createTestConfiguration()
			c.isDynamicUpstreams = test.dynamicUpstreams
			c.AddOrUpdateIngress(createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now))

			changes, problems := c.AddOrUpdateIngress(test.canary)

			if len(changes) != 0 {
				t.Errorf("changes %+v, want none", changes)
			}
			if len(problems) != 1 {
				t.Fatalf("problems %+v, want one", problems)
			}
			p := problems[0]
			if p.Object != test.canary || p.IsError || p.Reason != nl.EventReasonIgnored || !strings.HasPrefix(p.Message, test.wantMessagePrefix) {
				t.Errorf("problem %+v, want a warning starting with %q", p, test.wantMessagePrefix)
			}
		})
	}
}

func TestCanaryIngressOfMergeableIngress(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	now := time.Now()

	master := createTestIngressWithPath("cafe-master", "cafe.example.com", "/", "unused", now)
	master.Annotations["nginx.org/mergeable-ingress-type"] = "master"
	master.Spec.Rules[0].HTTP = nil
	minion := createTestIngressWithPath("cafe-tea", "cafe.example.com", "/tea", "tea-svc", now)
	minion.Annotations["nginx.org/mergeable-ingress-type"] = "minion"
	c.AddOrUpdateIngress(master)
	c.AddOrUpdateIngress(minion)

	canary := createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now, nil)
	changes, problems := c.AddOrUpdateIngress(canary)

	if len(problems) != 0 {
		t.Errorf("problems %+v, want none", problems)
	}
	if canaries := canariesOfChange(t, changes, "Ingress/default/cafe-master"); len(canaries) != 1 || canaries[0] != canary {
		t.Errorf("canaries of the master %v, want the canary", canaries)
	}
}

func TestIngressRequiresEndpointsUpdateForCanary(t *testing.T) {
	t.Parallel()
	lbc := LoadBalancerController{}
	now := time.Now()
	ingEx := &configs.IngressEx{
		Ingress: createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now),
		Canaries: []*configs.IngressEx{
			{Ingress: createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now, nil)},
		},
	}

	if !lbc.ingressRequiresEndpointsUpdate(ingEx, "tea-canary-svc") {
		t.Error("the endpoints of the canary's service need no update of the primary")
	}
	if lbc.ingressRequiresEndpointsUpdate(ingEx, "other-svc") {
		t.Error("the endpoints of another service need an update of the primary")
	}
}

// As ingress-nginx's alternative backend, a canary with -ingress-nginx-defaults is the canary of the upstream - namespace,
// Service, port - of the location it matches: every Ingress with a path to that Service has it, on any host and path.
func TestCanaryIngressPerUpstream(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	c.canaryPerUpstream = true
	now := time.Now()

	primary := createTestIngressWithPath("cafe", "cafe.example.com", "/tea", "tea-svc", now.Add(-3*time.Hour))
	other := createTestIngressWithPath("shop", "shop.example.com", "/drinks", "tea-svc", now.Add(-2*time.Hour))
	unrelated := createTestIngressWithPath("bar", "bar.example.com", "/tea", "beer-svc", now.Add(-2*time.Hour))
	c.AddOrUpdateIngress(primary)
	c.AddOrUpdateIngress(other)
	c.AddOrUpdateIngress(unrelated)

	canary := createTestCanaryIngress("cafe-canary", "cafe.example.com", "/tea", "tea-canary-svc", now, map[string]string{configs.CanaryByHeaderAnnotation: "x-canary"})
	changes, problems := c.AddOrUpdateIngress(canary)
	if len(problems) != 0 {
		t.Errorf("problems %+v, want none", problems)
	}
	for _, key := range []string{"Ingress/default/cafe", "Ingress/default/shop"} {
		canaries := canariesOfChange(t, changes, key)
		if len(canaries) != 1 || canaries[0] != canary {
			t.Errorf("canaries of %s %v, want the canary", key, canaries)
		}
	}
	for _, change := range changes {
		if change.Resource.GetKeyWithKind() == "Ingress/default/bar" {
			t.Errorf("the Ingress of another Service changed: %+v", change)
		}
		ingConfig := change.Resource.(*IngressConfiguration)
		cb, ok := ingConfig.CanaryBackends["default-tea-svc-80"]
		if !ok || cb.Ingress != canary || cb.Backend.Service.Name != "tea-canary-svc" || len(ingConfig.CanaryBackends) != 1 {
			t.Errorf("canary backends of %s %+v", change.Resource.GetKeyWithKind(), ingConfig.CanaryBackends)
		}
	}

	// a second canary of the same upstream is not used: ingress-nginx takes the first alternative backend
	second := createTestCanaryIngress("shop-canary", "shop.example.com", "/drinks", "tea-other-svc", now.Add(time.Minute), nil)
	c.AddOrUpdateIngress(second)
	for _, r := range c.GetResources() {
		ingConfig := r.(*IngressConfiguration)
		if cb, ok := ingConfig.CanaryBackends["default-tea-svc-80"]; ok && cb.Ingress != canary {
			t.Errorf("%s has the canary %s/%s, want the oldest", r.GetKeyWithKind(), cb.Ingress.Namespace, cb.Ingress.Name)
		}
	}

	// without the primary's location the canary is no one's
	changes, _ = c.DeleteIngress("default/cafe")
	if canaries := canariesOfChange(t, changes, "Ingress/default/shop"); len(canaries) != 1 || canaries[0] != second {
		t.Errorf("canaries of shop after the primary went %v, want the second canary, whose location is shop's", canaries)
	}
}
