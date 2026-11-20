package configs

import (
	"testing"

	"github.com/wx-one/wxingress/internal/nginx"
)

// countingManager counts reloads and the updates that go through the upstream API.
type countingManager struct {
	*nginx.FakeManager
	reloads int
	updates int
}

func (m *countingManager) Reload(isEndpointsUpdate bool) error {
	m.reloads++
	return m.FakeManager.Reload(isEndpointsUpdate)
}

func (m *countingManager) UpdateServersInPlus(upstream string, servers []string, config nginx.ServerConfig) error {
	m.updates++
	return m.FakeManager.UpdateServersInPlus(upstream, servers, config)
}

func TestUpdateEndpointsWithUpstreamAPIDoesNotReload(t *testing.T) {
	t.Parallel()
	manager := &countingManager{FakeManager: nginx.NewFakeManager("/etc/nginx")}
	cnf := createTestConfiguratorWithManager(t, manager)
	cnf.isUpstreamAPI = true

	if _, err := cnf.UpdateEndpoints([]*IngressEx{new(createCafeIngressEx())}); err != nil {
		t.Fatalf("UpdateEndpoints returned %v", err)
	}

	if manager.reloads != 0 {
		t.Errorf("reloads = %d, want 0", manager.reloads)
	}
	if manager.updates == 0 {
		t.Error("no upstream was updated through the API")
	}
}

func TestUpdateEndpointsWithoutUpstreamAPIReloads(t *testing.T) {
	t.Parallel()
	manager := &countingManager{FakeManager: nginx.NewFakeManager("/etc/nginx")}
	cnf := createTestConfiguratorWithManager(t, manager)

	if _, err := cnf.UpdateEndpoints([]*IngressEx{new(createCafeIngressEx())}); err != nil {
		t.Fatalf("UpdateEndpoints returned %v", err)
	}

	if manager.reloads != 1 {
		t.Errorf("reloads = %d, want 1", manager.reloads)
	}
	if manager.updates != 0 {
		t.Errorf("updates = %d, want 0", manager.updates)
	}
}

func TestUpstreamZoneSize(t *testing.T) {
	t.Parallel()
	tests := []struct {
		size        string
		upstreamAPI bool
		want        string
	}{
		{"0", false, "0"},
		{"0", true, "256k"},
		{"512k", true, "512k"},
		{"512k", false, "512k"},
	}
	for _, test := range tests {
		if got := upstreamZoneSize(test.size, test.upstreamAPI); got != test.want {
			t.Errorf("upstreamZoneSize(%q, %v) = %q, want %q", test.size, test.upstreamAPI, got, test.want)
		}
	}
}
