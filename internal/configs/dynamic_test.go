package configs

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/nginx"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConfigShapeLeavesOutTheServersTheModuleReaches(t *testing.T) {
	t.Parallel()
	config := func(servers string) []byte {
		return []byte("upstream u {\n\tzone u 256k;\n" + servers + "\tkeepalive 32;\n}\nserver {\n\tlisten 80;\n}\n")
	}
	one := config("\tserver 10.0.0.1:80 max_fails=1 fail_timeout=10s max_conns=0;\n")
	two := config("\tserver 10.0.0.2:80 max_fails=1 fail_timeout=10s max_conns=0;\n\tserver [fd00::1]:8080 max_fails=1 fail_timeout=10s max_conns=0;\n")
	none := config("\tserver 127.0.0.1:8181 max_fails=1 fail_timeout=10s max_conns=0;\n")
	socket := config("    server unix:/var/lib/nginx/nginx-502-server.sock max_fails=1 fail_timeout=10s max_conns=0;\n")
	params := config("\tserver 10.0.0.1:80 max_fails=3 fail_timeout=10s max_conns=0;\n")
	for name, other := range map[string][]byte{"other servers": two, "no endpoints": none, "the 502 socket": socket, "other parameters": params} {
		if configShape(one) != configShape(other) {
			t.Errorf("%s: the shape changed", name)
		}
	}

	// a server by name is resolved when the config is loaded, and other changes are changes
	for name, other := range map[string][]byte{
		"ExternalName":  config("\tserver example.com:80 max_fails=1 fail_timeout=10s max_conns=0;\n"),
		"keepalive":     []byte(string(one[:len(one)-30]) + "keepalive 64;\n}\nserver {\n\tlisten 80;\n}\n"),
		"server listen": config("\tserver 10.0.0.1:80 max_fails=1 fail_timeout=10s max_conns=0;\n")[:20],
	} {
		if configShape(one) == configShape(other) {
			t.Errorf("%s: the shape did not change", name)
		}
	}
}

func TestIngressUpdateReloadsOnlyWhenTheConfigChanges(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Fatalf("a new Ingress: reloads %d, want 1", manager.reloads)
	}

	// the same Ingress again, as a label or an unrelated annotation makes it: nothing
	manager.reset()
	ingEx.Ingress.Labels = map[string]string{"x": "y"}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.updated) != 0 {
		t.Errorf("no change: reloads %d, updated %v", manager.reloads, manager.updated)
	}

	// only the servers of tea changed: they go to the module, and only they
	manager.reset()
	ingEx.Endpoints["tea-svc80"] = []string{"10.0.0.2:80", "10.0.0.5:80"}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || !slices.Equal(manager.updated, []string{"default-cafe-ingress-cafe.example.com-tea-svc-80"}) {
		t.Errorf("new servers: reloads %d, updated %v", manager.reloads, manager.updated)
	}
	if config := manager.configs["default-cafe-ingress"]; !slicesContainsLine(config, "10.0.0.5:80") {
		t.Errorf("the file does not have the new servers for the next reload:\n%s", config)
	}

	// a change of the config reloads, and after it the module has what the file has
	manager.reset()
	ingEx.Ingress.Annotations["nginx.org/proxy-read-timeout"] = "33s"
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("a changed config: reloads %d, want 1", manager.reloads)
	}
	manager.reset()
	if _, err := cnf.AddOrUpdateResources(ExtendedResources{IngressExes: []*IngressEx{&ingEx}}, false); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.updated) != 0 {
		t.Errorf("after the reload: reloads %d, updated %v", manager.reloads, manager.updated)
	}

}

func TestExternalNameChangeReloads(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.CfgParams.ResolverAddresses = []string{"10.96.0.10"}

	ingEx := createCafeIngressEx()
	ingEx.Endpoints["tea-svc80"] = []string{"tea.example.com:80"}
	ingEx.ExternalNameSvcs["tea-svc"] = true
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}

	// a server by name is resolved when NGINX loads the config: another name is a change of the config
	manager.reset()
	ingEx.Endpoints["tea-svc80"] = []string{"tea.example.org:80"}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || len(manager.updated) != 0 {
		t.Errorf("another ExternalName: reloads %d, updated %v, want a reload", manager.reloads, manager.updated)
	}
}

func TestIngressUpdateWithoutTheModuleReloads(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.staticCfgParams.UpstreamAPI = false
	cnf.isUpstreamAPI = false

	ingEx := createCafeIngressEx()
	for range 2 {
		if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
			t.Fatal(err)
		}
	}
	if manager.reloads != 2 {
		t.Errorf("reloads %d, want 2: without the module every update reloads", manager.reloads)
	}
}

func TestVirtualServerUpdateReloadsOnlyWhenTheConfigChanges(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)

	vsEx := &VirtualServerEx{
		VirtualServer: &conf_v1.VirtualServer{
			ObjectMeta: meta_v1.ObjectMeta{Name: "cafe", Namespace: "default"},
			Spec: conf_v1.VirtualServerSpec{
				Host:      "cafe.example.com",
				Upstreams: []conf_v1.Upstream{{Name: "tea", Service: "tea-svc", Port: 80}},
				Routes:    []conf_v1.Route{{Path: "/", Action: &conf_v1.Action{Pass: "tea"}}},
			},
		},
		Endpoints: map[string][]string{"default/tea-svc:80": {"10.0.0.1:80"}},
	}
	if _, err := cnf.AddOrUpdateVirtualServer(vsEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Fatalf("a new VirtualServer: reloads %d, want 1", manager.reloads)
	}

	manager.reset()
	vsEx.Endpoints["default/tea-svc:80"] = []string{"10.0.0.1:80", "10.0.0.2:80"}
	if _, err := cnf.AddOrUpdateVirtualServer(vsEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || !slices.Equal(manager.updated, []string{"vs_default_cafe_tea"}) {
		t.Errorf("new servers: reloads %d, updated %v", manager.reloads, manager.updated)
	}

	manager.reset()
	vsEx.VirtualServer.Spec.Routes[0].Path = "/tea"
	if _, err := cnf.AddOrUpdateVirtualServer(vsEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("a changed config: reloads %d, want 1", manager.reloads)
	}
}

func slicesContainsLine(config, text string) bool {
	return slices.ContainsFunc(splitLines(config), func(line string) bool { return len(line) > 0 && containsText(line, text) })
}

func splitLines(s string) []string { return strings.Split(s, "\n") }

func containsText(s, text string) bool { return strings.Contains(s, text) }

// A canary rule that changes in a batch waits for its end, since it may name
// an upstream the reload at the end brings; a batch that ends without one
// gives it to the module then.
func TestCanaryOfABatchThatEndsWithoutAReload(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)

	ingEx := createCafeIngressEx()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}

	manager.reset()
	cnf.DisableReloads()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "50"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if len(manager.set) != 0 || cnf.ReloadAsked() {
		t.Fatalf("in the batch: set %v, reload asked %v, want neither", manager.set, cnf.ReloadAsked())
	}
	cnf.EnableReloads()
	if err := cnf.ReloadForBatchUpdates(false); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.set) != 1 || cnf.appliedCanaries[cafeTeaUpstream].Weight != 50 {
		t.Errorf("after the batch: reloads %d, set %v, applied weight %d", manager.reloads, manager.set, cnf.appliedCanaries[cafeTeaUpstream].Weight)
	}

	// a batch without canary changes gives nothing
	manager.reset()
	cnf.DisableReloads()
	cnf.EnableReloads()
	if err := cnf.ReloadForBatchUpdates(false); err != nil {
		t.Fatal(err)
	}
	if len(manager.set) != 0 || len(manager.deleted) != 0 {
		t.Errorf("an empty batch: set %v, deleted %v", manager.set, manager.deleted)
	}
}

// A reload asked for within the reload interval waits, and so do the canary
// rules, which may name an upstream it brings.
func TestReloadWaitsForTheIntervalAndCanariesWithIt(t *testing.T) {
	t.Parallel()
	manager := newCanaryManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.reloadInterval = time.Hour

	ingEx := createCafeIngressEx()
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Fatalf("the first reload: %d, want it at once", manager.reloads)
	}

	manager.reset()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.set) != 0 || !cnf.ReloadAsked() {
		t.Fatalf("a new canary within the interval: reloads %d, set %v, asked %v, want it to wait", manager.reloads, manager.set, cnf.ReloadAsked())
	}
	if got := cnf.NextReload(); time.Until(got) < 59*time.Minute {
		t.Errorf("the next reload is at %v, want an hour after the last", got)
	}

	if err := cnf.ReloadIfAsked(); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || len(manager.set) != 1 || cnf.ReloadAsked() {
		t.Errorf("the reload that waited: reloads %d, set %v, asked %v", manager.reloads, manager.set, cnf.ReloadAsked())
	}
	if err := cnf.ReloadIfAsked(); err != nil || manager.reloads != 1 {
		t.Errorf("nothing asked: reloads %d, %v", manager.reloads, err)
	}
}

func TestReloadInterval(t *testing.T) {
	t.Parallel()
	for rate, want := range map[float64]time.Duration{0: 0, -1: 0, 0.5: 2 * time.Second, 2: 500 * time.Millisecond} {
		if got := reloadInterval(rate); got != want {
			t.Errorf("reloadInterval(%v) = %v, want %v", rate, got, want)
		}
	}
}

type refusingCanaryManager struct {
	*canaryManager
	refuse bool
}

func (m *refusingCanaryManager) SetCanary(primary string, canary nginx.Canary) error {
	if m.refuse {
		return errors.New("refused")
	}
	return m.canaryManager.SetCanary(primary, canary)
}

func TestRefusedCanaryIsRetried(t *testing.T) {
	t.Parallel()
	manager := &refusingCanaryManager{canaryManager: newCanaryManager(), refuse: true}
	cnf := createCanaryConfigurator(t, manager)

	ingEx := createCafeIngressEx()
	ingEx.Canaries = []*IngressEx{createCafeCanaryIngressEx(map[string]string{CanaryWeightAnnotation: "10"})}
	if _, err := cnf.AddOrUpdateIngress(&ingEx); err != nil {
		t.Fatal(err)
	}
	if !cnf.PushesFailed() {
		t.Fatal("a refused canary rule is not remembered")
	}
	manager.refuse = false
	if err := cnf.RetryPushes(); err != nil {
		t.Fatal(err)
	}
	if cnf.PushesFailed() || len(manager.set) != 1 {
		t.Errorf("after the retry: failed %v, set %v", cnf.PushesFailed(), manager.set)
	}
}

// mainConfigManager tells if the main config changed.
type mainConfigManager struct {
	*canaryManager
	main string
}

func (m *mainConfigManager) CreateMainConfig(content []byte) (bool, error) {
	changed := m.main != string(content)
	m.main = string(content)
	return changed, nil
}

// A ConfigMap change that renders the same configs reloads nothing.
func TestUpdateConfigReloadsOnlyWhenAConfigChanged(t *testing.T) {
	t.Parallel()
	manager := &mainConfigManager{canaryManager: newCanaryManager()}
	cnf := createCanaryConfigurator(t, manager)
	ingEx := createCafeIngressEx()
	resources := ExtendedResources{IngressExes: []*IngressEx{&ingEx}}

	// the first time - NGINX starts with the controller's configs - always reloads
	if _, _, err := cnf.UpdateConfig(resources); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Fatalf("the first UpdateConfig: %d reloads, want 1", manager.reloads)
	}

	// a key that renders the same
	manager.reset()
	cnf.CfgParams.ProxyConnectTimeout = cnf.CfgParams.ProxyConnectTimeout + ""
	if _, _, err := cnf.UpdateConfig(resources); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 {
		t.Errorf("nothing changed: %d reloads, want none", manager.reloads)
	}

	// new servers alone go to the module
	manager.reset()
	ingEx.Endpoints["tea-svc80"] = []string{"10.0.0.2:80", "10.0.0.7:80"}
	if _, _, err := cnf.UpdateConfig(resources); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.updated) != 1 {
		t.Errorf("new servers: %d reloads, updated %v", manager.reloads, manager.updated)
	}

	// a key that changes an Ingress config
	manager.reset()
	cnf.CfgParams.ProxyReadTimeout = "61s"
	if _, _, err := cnf.UpdateConfig(resources); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("a changed Ingress config: %d reloads, want 1", manager.reloads)
	}

	// a key that changes the main config
	manager.reset()
	cnf.CfgParams.MainWorkerConnections = "4096"
	if _, _, err := cnf.UpdateConfig(resources); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("a changed main config: %d reloads, want 1", manager.reloads)
	}
}

// streamManager records, beside what canaryManager does, the stream configs and
// the servers of stream upstreams that go through the upstream API.
type streamManager struct {
	*canaryManager
	streamConfigs map[string]string
	streamUpdated []string
	refuseStream  bool
}

func newStreamManager() *streamManager {
	return &streamManager{canaryManager: newCanaryManager(), streamConfigs: make(map[string]string)}
}

func (m *streamManager) CreateStreamConfig(name string, content []byte) (bool, error) {
	changed := m.streamConfigs[name] != string(content)
	m.streamConfigs[name] = string(content)
	return changed, nil
}

func (m *streamManager) UpdateStreamServersInPlus(upstream string, servers []string, config nginx.ServerConfig) error {
	if m.refuseStream {
		return errors.New("refused")
	}
	m.streamUpdated = append(m.streamUpdated, fmt.Sprintf("%s %s max_fails=%d fail_timeout=%s", upstream, strings.Join(servers, ","), config.MaxFails, config.FailTimeout))
	return nil
}

func (m *streamManager) reset() {
	m.canaryManager.reset()
	m.streamUpdated = nil
}

func createTCPTransportServerEx(endpoints ...string) *TransportServerEx {
	maxFails := 3
	return &TransportServerEx{
		ListenerPort: 5353,
		TransportServer: &conf_v1.TransportServer{
			ObjectMeta: meta_v1.ObjectMeta{Name: "tcp-app", Namespace: "default"},
			Spec: conf_v1.TransportServerSpec{
				Listener:  conf_v1.TransportServerListener{Name: "tcp-listener", Protocol: "TCP"},
				Upstreams: []conf_v1.TransportServerUpstream{{Name: "app", Service: "app", Port: 8080, MaxFails: &maxFails, FailTimeout: "30s"}},
				Action:    &conf_v1.TransportServerAction{Pass: "app"},
			},
		},
		Endpoints: map[string][]string{GenerateEndpointsKey("default", "app", nil, 8080): endpoints},
	}
}

func TestTransportServerEndpointsGoToTheModuleWithoutAReload(t *testing.T) {
	t.Parallel()
	manager := newStreamManager()
	cnf := createCanaryConfigurator(t, manager)
	const upstream = "ts_default_tcp-app_app"
	const file = "ts_default_tcp-app"

	if _, err := cnf.AddOrUpdateTransportServer(createTCPTransportServerEx("10.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Fatalf("a new TransportServer: reloads %d, want 1", manager.reloads)
	}

	// a pod more: its server goes to the module, with the upstream's parameters, and nothing is reloaded
	manager.reset()
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{createTCPTransportServerEx("10.0.0.1:8080", "10.0.0.2:8080")}); err != nil {
		t.Fatal(err)
	}
	want := []string{upstream + " 10.0.0.1:8080,10.0.0.2:8080 max_fails=3 fail_timeout=30s"}
	if manager.reloads != 0 || !slices.Equal(manager.streamUpdated, want) {
		t.Errorf("new endpoints: reloads %d, stream updates %v, want 0 and %v", manager.reloads, manager.streamUpdated, want)
	}
	if strings.Contains(manager.streamConfigs[file], "10.0.0.2:8080") {
		t.Errorf("the config was written at once rather than before the next reload:\n%s", manager.streamConfigs[file])
	}
	if cnf.ReloadAsked() {
		t.Error("new endpoints asked for a reload")
	}

	// the same endpoints again: the module has them
	manager.reset()
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{createTCPTransportServerEx("10.0.0.2:8080", "10.0.0.1:8080")}); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.streamUpdated) != 0 {
		t.Errorf("the same endpoints: reloads %d, stream updates %v", manager.reloads, manager.streamUpdated)
	}

	// an update of the TransportServer that changes nothing but its endpoints reloads nothing either
	manager.reset()
	tsEx := createTCPTransportServerEx("10.0.0.2:8080")
	tsEx.TransportServer.Labels = map[string]string{"x": "y"}
	if _, err := cnf.AddOrUpdateTransportServer(tsEx); err != nil {
		t.Fatal(err)
	}
	want = []string{upstream + " 10.0.0.2:8080 max_fails=3 fail_timeout=30s"}
	if manager.reloads != 0 || !slices.Equal(manager.streamUpdated, want) {
		t.Errorf("a label and endpoints: reloads %d, stream updates %v, want 0 and %v", manager.reloads, manager.streamUpdated, want)
	}

	// no pods: the module gets none, and the file the socket that stands for none
	manager.reset()
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{createTCPTransportServerEx()}); err != nil {
		t.Fatal(err)
	}
	want = []string{upstream + "  max_fails=3 fail_timeout=30s"}
	if manager.reloads != 0 || !slices.Equal(manager.streamUpdated, want) {
		t.Errorf("no endpoints: reloads %d, stream updates %v, want 0 and %v", manager.reloads, manager.streamUpdated, want)
	}

	// a change of the config reloads, and the staged file is written with the servers the module has
	manager.reset()
	tsEx = createTCPTransportServerEx("10.0.0.3:8080")
	tsEx.TransportServer.Spec.Upstreams[0].FailTimeout = "40s"
	tsEx.TransportServer.Spec.Upstreams[0].LoadBalancingMethod = "least_conn"
	if _, err := cnf.AddOrUpdateTransportServer(tsEx); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("a changed config: reloads %d, want 1", manager.reloads)
	}
	if !strings.Contains(manager.streamConfigs[file], "server 10.0.0.3:8080 max_fails=3 fail_timeout=40s") {
		t.Errorf("the file does not have the servers of the reload:\n%s", manager.streamConfigs[file])
	}

	// after the reload the module has what the file has
	manager.reset()
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{tsEx}); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 0 || len(manager.streamUpdated) != 0 {
		t.Errorf("after the reload: reloads %d, stream updates %v", manager.reloads, manager.streamUpdated)
	}

	// a server the module does not take is a reload, as for http upstreams
	manager.reset()
	manager.refuseStream = true
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{createTCPTransportServerEx("10.0.0.4:8080")}); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 {
		t.Errorf("refused: reloads %d, want 1", manager.reloads)
	}
	if !strings.Contains(manager.streamConfigs[file], "10.0.0.4:8080") {
		t.Errorf("the reload does not have the servers the module refused:\n%s", manager.streamConfigs[file])
	}
}

func TestStreamAndHTTPUpstreamsOfOneNameAreApart(t *testing.T) {
	t.Parallel()
	manager := newStreamManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.lastReload = time.Now()

	if err := cnf.updateServers("same", []string{"10.0.0.1:80"}, nginx.ServerConfig{MaxFails: 1, FailTimeout: "10s"}); err != nil {
		t.Fatal(err)
	}
	if err := cnf.updateStreamServers("same", []string{"10.0.0.1:80"}, nginx.ServerConfig{MaxFails: 1, FailTimeout: "10s"}); err != nil {
		t.Fatal(err)
	}
	if len(manager.updated) != 1 || len(manager.streamUpdated) != 1 {
		t.Errorf("http updates %v, stream updates %v: want one each", manager.updated, manager.streamUpdated)
	}
}

func TestTransportServerEndpointsWithoutTheModuleReload(t *testing.T) {
	t.Parallel()
	manager := newStreamManager()
	cnf := createCanaryConfigurator(t, manager)
	cnf.staticCfgParams.UpstreamAPI = false
	cnf.isUpstreamAPI = false

	if _, err := cnf.AddOrUpdateTransportServer(createTCPTransportServerEx("10.0.0.1:8080")); err != nil {
		t.Fatal(err)
	}
	manager.reset()
	if err := cnf.UpdateEndpointsForTransportServers([]*TransportServerEx{createTCPTransportServerEx("10.0.0.2:8080")}); err != nil {
		t.Fatal(err)
	}
	if manager.reloads != 1 || len(manager.streamUpdated) != 0 {
		t.Errorf("without the module: reloads %d, stream updates %v, want a reload", manager.reloads, manager.streamUpdated)
	}
	if !strings.Contains(manager.streamConfigs["ts_default_tcp-app"], "10.0.0.2:8080") {
		t.Errorf("the config was not written with the new endpoints:\n%s", manager.streamConfigs["ts_default_tcp-app"])
	}
}
