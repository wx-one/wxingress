package configs

import (
	"slices"
	"testing"

	"github.com/wx-one/wxingress/internal/nginx"
)

// balancerManager counts reloads and the balancer rules set and deleted.
type balancerManager struct {
	*canaryManager
	balancers []string
	removed   []string
}

func (m *balancerManager) SetBalancer(upstream string, balancer nginx.Balancer) error {
	m.balancers = append(m.balancers, upstream+" "+balancer.Algorithm)
	return nil
}

func (m *balancerManager) DeleteBalancer(upstream string) error {
	m.removed = append(m.removed, upstream)
	return nil
}

func (m *balancerManager) reset() {
	m.canaryManager.reset()
	m.balancers = nil
	m.removed = nil
}

func TestConfiguratorBalancersWithoutReloads(t *testing.T) {
	t.Parallel()
	manager := &balancerManager{canaryManager: newCanaryManager()}
	cnf := createCanaryConfigurator(t, manager)
	const coffee = "default_cafe-ingress_cafe.example.com_coffee-svc_80"

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || len(manager.balancers) != 0 {
		t.Fatalf("first: reloads %d, balancers %v", manager.reloads, manager.balancers)
	}

	// each annotation of ingress-nginx's balancing in turn: a rule, never a reload
	steps := []struct {
		annotation, value, algorithm string
	}{
		{IngressNginxLoadBalanceAnnotation, "ewma", "ewma"},
		{IngressNginxLoadBalanceAnnotation, "round_robin", "round_robin"},
		{IngressNginxUpstreamHashByAnnotation, "$request_uri", "chash"},
		{IngressNginxUpstreamHashBySubsetAnnotation, "true", "chashsubset"},
		{IngressNginxAffinityAnnotation, "cookie", "sticky_balanced"},
		{IngressNginxAffinityModeAnnotation, "persistent", "sticky_persistent"},
	}
	for _, step := range steps {
		manager.reset()
		ingEx.Ingress.Annotations[step.annotation] = step.value
		if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
			t.Fatal(err)
		}
		if manager.reloads != 0 {
			t.Errorf("%s %s: reloads = %d, want 0", step.annotation, step.value, manager.reloads)
		}
		if !slices.Contains(manager.balancers, coffee+" "+step.algorithm) {
			t.Errorf("%s %s: set %v, want %s", step.annotation, step.value, manager.balancers, step.algorithm)
		}
	}

	// a cookie attribute alone is a new rule too
	manager.reset()
	ingEx.Ingress.Annotations[IngressNginxSessionCookieMaxAgeAnnotation] = "60"
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.balancers) != 2 {
		t.Errorf("max-age: reloads %d, set %v", manager.reloads, manager.balancers)
	}

	// and all of them gone: the rules are deleted, still without a reload
	manager.reset()
	for _, step := range steps {
		delete(ingEx.Ingress.Annotations, step.annotation)
	}
	delete(ingEx.Ingress.Annotations, IngressNginxSessionCookieMaxAgeAnnotation)
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || !slices.Contains(manager.removed, coffee) || len(cnf.appliedBalancers) != 0 {
		t.Errorf("none: reloads %d, removed %v, applied %v", manager.reloads, manager.removed, cnf.appliedBalancers)
	}

	// the Ingress goes: its rules go with the reload
	manager.reset()
	ingEx.Ingress.Annotations[IngressNginxLoadBalanceAnnotation] = "ewma"
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	manager.reset()
	if err := cnf.DeleteIngress(ingEx.Ingress.Namespace+"/"+ingEx.Ingress.Name, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(manager.removed, coffee) {
		t.Errorf("deleted: removed %v", manager.removed)
	}
}
