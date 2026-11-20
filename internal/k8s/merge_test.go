package k8s

import (
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	networking "k8s.io/api/networking/v1"
)

func holderOf(t *testing.T, c *Configuration, host string) *IngressConfiguration {
	t.Helper()
	ingConfig, ok := c.hosts[host].(*IngressConfiguration)
	if !ok {
		t.Fatalf("host %s is held by %v, not by an Ingress", host, c.hosts[host])
	}
	return ingConfig
}

func TestIngressesOfAHostAreMerged(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()

	older := createTestIngressWithPath("gitlab", "gitlab.example.com", "/", "webservice", now.Add(-time.Hour))
	newer := createTestIngressWithPath("gitlab-direct", "gitlab.example.com", "/api", "direct", now)
	newer.Namespace = "other"

	c.AddOrUpdateIngress(newer)
	changes, problems := c.AddOrUpdateIngress(older)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}

	holder := holderOf(t, c, "gitlab.example.com")
	if holder.Ingress != older {
		t.Fatalf("the host is held by %s, want the older Ingress", holder.Ingress.Name)
	}
	if len(holder.Merged) != 1 || holder.Merged[0].Ingress != newer || holder.Merged[0].Host != "gitlab.example.com" {
		t.Fatalf("merged: %v", holder.Merged)
	}
	if len(holder.Warnings) != 0 {
		t.Errorf("warnings of the holder: %v", holder.Warnings)
	}

	// the newer one loses its own config, which held the host before
	var deleted, updated bool
	for _, change := range changes {
		switch {
		case change.Op == Delete && change.Resource.GetObjectMeta() == &newer.ObjectMeta:
			deleted = true
		case change.Op == AddOrUpdate && change.Resource.GetObjectMeta() == &older.ObjectMeta:
			updated = true
		}
	}
	if !deleted || !updated {
		t.Errorf("changes %v: want the config of the newer deleted and the older updated", changes)
	}

	// a change of the merged Ingress changes the config of the holder
	changed := newer.DeepCopy()
	changed.Generation++
	changes, problems = c.AddOrUpdateIngress(changed)
	if len(problems) != 0 || len(changes) != 1 || changes[0].Resource.GetObjectMeta() != &older.ObjectMeta {
		t.Errorf("changes %v and problems %v after the merged Ingress changed", changes, problems)
	}

	// its services are upstreams of the holder
	resources := c.FindResourcesForService("other", "direct")
	if len(resources) != 1 || resources[0].GetObjectMeta() != &older.ObjectMeta {
		t.Errorf("resources of the service of the merged Ingress: %v", resources)
	}

	// without the older one the newer one holds the host again
	changes, problems = c.DeleteIngress("default/gitlab")
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	if holder := holderOf(t, c, "gitlab.example.com"); holder.Ingress.Name != "gitlab-direct" || len(holder.Merged) != 0 {
		t.Errorf("after the delete the host is held by %s with %v merged", holder.Ingress.Name, holder.Merged)
	}
	if len(changes) != 2 {
		t.Errorf("changes: %v", changes)
	}
}

func TestIngressWithSeveralHostsKeepsTheHostsItWins(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()

	older := createTestIngressWithPath("a", "a.example.com", "/", "svc-a", now.Add(-time.Hour))
	newer := createTestIngressWithPath("b", "a.example.com", "/b", "svc-b", now)
	newer.Spec.Rules = append(newer.Spec.Rules, networking.IngressRule{Host: "b.example.com", IngressRuleValue: newer.Spec.Rules[0].IngressRuleValue})

	c.AddOrUpdateIngress(older)
	_, problems := c.AddOrUpdateIngress(newer)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}

	if holder := holderOf(t, c, "b.example.com"); holder.Ingress != newer {
		t.Fatalf("b.example.com is held by %s", holder.Ingress.Name)
	}
	b := holderOf(t, c, "b.example.com")
	if !b.ValidHosts["b.example.com"] || b.ValidHosts["a.example.com"] {
		t.Errorf("valid hosts of the newer one: %v", b.ValidHosts)
	}
	if holder := holderOf(t, c, "a.example.com"); len(holder.Merged) != 1 || holder.Merged[0].Ingress != newer {
		t.Errorf("merged into a.example.com: %v", holder.Merged)
	}
}

func TestIngressesOfAHostAreNotMergedIntoAVirtualServer(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	now := time.Now()

	vs := createTestVirtualServer("vs", "foo.example.com")
	vs.CreationTimestamp.Time = now.Add(-2 * time.Hour)
	first := createTestIngressWithPath("first", "foo.example.com", "/", "svc", now.Add(-time.Hour))
	second := createTestIngressWithPath("second", "foo.example.com", "/b", "svc", now)

	c.AddOrUpdateVirtualServer(vs)
	c.AddOrUpdateIngress(first)
	c.AddOrUpdateIngress(second)

	if _, isVS := c.hosts["foo.example.com"].(*VirtualServerConfiguration); !isVS {
		t.Fatalf("the host is held by %v", c.hosts["foo.example.com"])
	}
	for _, key := range []string{"Ingress/default/first", "Ingress/default/second"} {
		if p, exists := c.hostProblems[key]; !exists || p.Message != "All hosts are taken by other resources" {
			t.Errorf("problem of %s: %+v", key, p)
		}
	}
}

func TestCanaryAttachesToThePathOfAMergedIngress(t *testing.T) {
	t.Parallel()
	c := createTestCanaryConfiguration()
	now := time.Now()

	direct := createTestIngressWithPath("gitlab-direct", "gitlab.example.com", "/api", "direct", now.Add(-time.Hour))
	chart := createTestIngressWithPath("gitlab-chart", "gitlab.example.com", "/", "webservice", now)
	canary := createTestCanaryIngress("gitlab-waf", "gitlab.example.com", "/", "waf", now, map[string]string{configs.CanaryWeightAnnotation: "100"})
	canary.Namespace = "waf"

	c.AddOrUpdateIngress(direct)
	c.AddOrUpdateIngress(chart)
	_, problems := c.AddOrUpdateIngress(canary)
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}

	holder := holderOf(t, c, "gitlab.example.com")
	if holder.Ingress != direct || len(holder.Merged) != 1 || len(holder.Canaries) != 1 || holder.Canaries[0] != canary {
		t.Errorf("holder %s, merged %v, canaries %v", holder.Ingress.Name, holder.Merged, holder.Canaries)
	}
}
