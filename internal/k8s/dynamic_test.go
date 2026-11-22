package k8s

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/configs"
	"github.com/wx-one/wxingress/internal/configs/version1"
	"github.com/wx-one/wxingress/internal/configs/version2"
	"github.com/wx-one/wxingress/internal/nginx"
	discovery_v1 "k8s.io/api/discovery/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/cache"

	"github.com/wx-one/wxingress/internal/k8s/secrets"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSecretRolesNeedReload(t *testing.T) {
	t.Parallel()
	tests := []struct {
		roles      []secrets.SecretRole
		dynamicSSL bool
		perRequest bool
		want       bool
	}{
		{nil, true, true, true},
		{[]secrets.SecretRole{secrets.RoleHtpasswd}, false, false, true},
		{[]secrets.SecretRole{secrets.RoleHtpasswd}, false, true, false},
		{[]secrets.SecretRole{secrets.RoleAuthFile}, false, true, false},
		{[]secrets.SecretRole{secrets.RoleAuthMap}, false, true, false},
		{[]secrets.SecretRole{secrets.RoleAuthMap, secrets.RoleTLS}, false, true, true},
		{[]secrets.SecretRole{secrets.RoleAuthMap, secrets.RoleTLS}, true, true, false},
		{[]secrets.SecretRole{secrets.RoleAuthFile, secrets.RoleCA}, true, true, true},
		{[]secrets.SecretRole{secrets.RoleTLS}, true, false, false},
		{[]secrets.SecretRole{secrets.RoleJWK}, true, true, false},
		{[]secrets.SecretRole{secrets.RoleJWK}, true, false, true},
	}
	for _, tc := range tests {
		if got := secretRolesNeedReload(tc.roles, tc.dynamicSSL, tc.perRequest); got != tc.want {
			t.Errorf("secretRolesNeedReload(%v, %v, %v) = %v, want %v", tc.roles, tc.dynamicSSL, tc.perRequest, got, tc.want)
		}
	}
}

func TestServiceNeedsReload(t *testing.T) {
	t.Parallel()
	clusterIP := &api_v1.Service{Spec: api_v1.ServiceSpec{Type: api_v1.ServiceTypeClusterIP}}
	externalName := &api_v1.Service{Spec: api_v1.ServiceSpec{Type: api_v1.ServiceTypeExternalName, ExternalName: "example.com"}}
	tests := []struct {
		dynamic bool
		obj     interface{}
		exists  bool
		want    bool
	}{
		{false, clusterIP, true, true},
		{false, nil, false, true},
		{true, clusterIP, true, false},
		{true, nil, false, false},
		{true, externalName, true, true},
	}
	for i, tc := range tests {
		if got := serviceNeedsReload(tc.dynamic, tc.obj, tc.exists); got != tc.want {
			t.Errorf("%d: serviceNeedsReload = %v, want %v", i, got, tc.want)
		}
	}
}

func TestHasHeadlessServiceChanged(t *testing.T) {
	t.Parallel()
	headless := func(mutate func(*api_v1.Service)) *api_v1.Service {
		svc := &api_v1.Service{
			ObjectMeta: meta_v1.ObjectMeta{Name: "h", Namespace: "default", Labels: map[string]string{"a": "1"}},
			Spec: api_v1.ServiceSpec{
				ClusterIP:  api_v1.ClusterIPNone,
				ClusterIPs: []string{api_v1.ClusterIPNone},
				Ports:      []api_v1.ServicePort{{Name: "http", Port: 80}},
			},
		}
		if mutate != nil {
			mutate(svc)
		}
		return svc
	}
	tests := []struct {
		name   string
		mutate func(*api_v1.Service)
		want   bool
	}{
		{"labels", func(s *api_v1.Service) { s.Labels["a"] = "2" }, false},
		{"annotations", func(s *api_v1.Service) { s.Annotations = map[string]string{"x": "y"} }, false},
		{"selector", func(s *api_v1.Service) { s.Spec.Selector = map[string]string{"app": "other"} }, false},
		{"port", func(s *api_v1.Service) { s.Spec.Ports[0].Port = 8080 }, true},
		{"cluster IP", func(s *api_v1.Service) { s.Spec.ClusterIP = "10.0.0.1"; s.Spec.ClusterIPs = []string{"10.0.0.1"} }, true},
		{"type", func(s *api_v1.Service) {
			s.Spec.Type = api_v1.ServiceTypeExternalName
			s.Spec.ExternalName = "example.com"
		}, true},
	}
	for _, tc := range tests {
		old, cur := headless(nil), headless(tc.mutate)
		if !hasServicedChanged(old, cur) {
			t.Fatalf("%s: hasServicedChanged lets no headless update through", tc.name)
		}
		if got := hasHeadlessServiceChanged(old, cur); got != tc.want {
			t.Errorf("%s: hasHeadlessServiceChanged = %v, want %v", tc.name, got, tc.want)
		}
	}

	// a Service that is not headless is decided by hasServicedChanged alone
	if !hasHeadlessServiceChanged(&api_v1.Service{}, &api_v1.Service{}) {
		t.Error("an update of a Service with a cluster IP is held back")
	}
}

// endpointsManager tells if a config changed, and records reloads and what
// reaches the upstream API.
type endpointsManager struct {
	*nginx.FakeManager
	written map[string]string
	configs int
	reloads int
	servers map[string][]string
	updates []string
	// refuse makes the upstream API refuse every update
	refuse bool
}

func newEndpointsManager() *endpointsManager {
	return &endpointsManager{FakeManager: nginx.NewFakeManager("/etc/nginx"), written: make(map[string]string), servers: make(map[string][]string)}
}

func (m *endpointsManager) CreateConfig(name string, content []byte) (bool, error) {
	if m.written[name] == string(content) {
		return false, nil
	}
	m.configs++
	m.written[name] = string(content)
	return true, nil
}

func (m *endpointsManager) Reload(_ bool) error {
	m.reloads++
	return nil
}

func (m *endpointsManager) UpdateServersInPlus(upstream string, servers []string, _ nginx.ServerConfig) error {
	if m.refuse {
		return fmt.Errorf("upstream %v not found", upstream)
	}
	m.updates = append(m.updates, upstream)
	m.servers[upstream] = slices.Sorted(slices.Values(servers))
	return nil
}

func (m *endpointsManager) reset() {
	m.configs, m.reloads, m.updates = 0, 0, nil
}

const teaUpstream = "default_cafe_cafe.example.com_tea_80"

func teaSlice(name string, ips ...string) *discovery_v1.EndpointSlice {
	slice := &discovery_v1.EndpointSlice{
		ObjectMeta:  meta_v1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{discovery_v1.LabelServiceName: "tea"}},
		AddressType: discovery_v1.AddressTypeIPv4,
		Ports:       []discovery_v1.EndpointPort{{Port: new(int32(8080))}},
	}
	for _, ip := range ips {
		slice.Endpoints = append(slice.Endpoints, discovery_v1.Endpoint{Addresses: []string{ip}, Conditions: discovery_v1.EndpointConditions{Ready: new(true)}})
	}
	return slice
}

// newEndpointsTestLBC is a controller with -dynamic-upstreams and an Ingress
// cafe.example.com of the Service tea, whose EndpointSlice tea-1 has 10.0.0.1.
func newEndpointsTestLBC(t *testing.T, syncRateLimit ...float64) (*LoadBalancerController, *endpointsManager, cache.Store) {
	t.Helper()
	manager := newEndpointsManager()
	lbc := newBatchTestLBC(t, manager)
	lbc.configuration.isDynamicUpstreams = true

	templateExecutor, err := version1.NewTemplateExecutor(
		filepath.Join("..", "configs", "version1", "nginx.tmpl"),
		filepath.Join("..", "configs", "version1", "nginx.ingress.tmpl"),
	)
	if err != nil {
		t.Fatal(err)
	}
	templateExecutorV2, err := version2.NewTemplateExecutor(
		filepath.Join("..", "configs", "version2", "nginx.virtualserver.tmpl"),
		filepath.Join("..", "configs", "version2", "nginx.transportserver.tmpl"),
		filepath.Join("..", "configs", "version2", "oidc.tmpl"),
	)
	if err != nil {
		t.Fatal(err)
	}
	lbc.configurator = configs.NewConfigurator(configs.ConfiguratorParams{
		NginxManager:       manager,
		StaticCfgParams:    &configs.StaticConfigParams{NginxVersion: nginx.NewVersion("nginx version: nginx/1.25.3"), UpstreamAPI: true},
		Config:             configs.NewDefaultConfigParams(context.Background(), false),
		MGMTCfgParams:      configs.NewDefaultMGMTConfigParams(context.Background()),
		TemplateExecutor:   templateExecutor,
		TemplateExecutorV2: templateExecutorV2,
		IsDynamicUpstreams: true,
		SyncRateLimit:      append(syncRateLimit, 0)[0],
	})
	lbc.configurator.EnableReloads()
	lbc.secretStore = secrets.NewLocalSecretStore(lbc.configurator, secrets.WithSecretResolver(lbc.getSecret))
	lbc.areCustomResourcesEnabled = false
	lbc.statusUpdater = &statusUpdater{namespace: "nginx-ingress", externalServiceName: "nginx-ingress", logger: lbc.Logger}

	nsi := lbc.namespacedInformers.Get("default")
	svcStore := cache.NewStore(cache.MetaNamespaceKeyFunc)
	if err := svcStore.Add(&api_v1.Service{
		ObjectMeta: meta_v1.ObjectMeta{Name: "tea", Namespace: "default"},
		Spec:       api_v1.ServiceSpec{Ports: []api_v1.ServicePort{{Port: 80, TargetPort: intstr.FromInt32(8080)}}},
	}); err != nil {
		t.Fatal(err)
	}
	nsi.svcLister = svcStore
	if err := nsi.endpointSliceLister.Add(teaSlice("tea-1", "10.0.0.1")); err != nil {
		t.Fatal(err)
	}
	lbc.configuration.CompleteStartup()

	ing := &networking.Ingress{
		ObjectMeta: meta_v1.ObjectMeta{
			Name: "cafe", Namespace: "default", CreationTimestamp: meta_v1.Now(),
			Annotations: map[string]string{"kubernetes.io/ingress.class": "nginx"},
		},
		Spec: networking.IngressSpec{Rules: []networking.IngressRule{{
			Host: "cafe.example.com",
			IngressRuleValue: networking.IngressRuleValue{HTTP: &networking.HTTPIngressRuleValue{Paths: []networking.HTTPIngressPath{{
				Path: "/", PathType: new(networking.PathTypePrefix),
				Backend: networking.IngressBackend{Service: &networking.IngressServiceBackend{Name: "tea", Port: networking.ServiceBackendPort{Number: 80}}},
			}}}},
		}}},
	}
	if _, problems := lbc.configuration.AddOrUpdateIngress(ing); len(problems) > 0 {
		t.Fatalf("AddOrUpdateIngress() problems = %v", problems)
	}
	if _, err := lbc.configurator.AddOrUpdateResources(lbc.createExtendedResources(lbc.configuration.GetResources()), true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(manager.written["default_cafe"], "server 10.0.0.1:8080") {
		t.Fatalf("the config has no server of tea:\n%s", manager.written["default_cafe"])
	}
	return lbc, manager, nsi.endpointSliceLister.Store
}

func TestEndpointSlicesOfAServiceGoToTheModuleTogether(t *testing.T) {
	t.Parallel()
	lbc, manager, sliceStore := newEndpointsTestLBC(t)
	handlers := createEndpointSliceHandlers(lbc)

	// the Service gets a second EndpointSlice and its first one changes: one task, for the Service
	manager.reset()
	second := teaSlice("tea-2", "10.0.0.3")
	if err := sliceStore.Add(second); err != nil {
		t.Fatal(err)
	}
	handlers.OnAdd(second, false)
	old, changed := teaSlice("tea-1", "10.0.0.1"), teaSlice("tea-1", "10.0.0.1", "10.0.0.2")
	if err := sliceStore.Update(changed); err != nil {
		t.Fatal(err)
	}
	handlers.OnUpdate(old, changed)
	if got := lbc.syncQueue.queue.Len(); got != 1 {
		t.Fatalf("%d tasks queued for two EndpointSlices of one Service, want 1", got)
	}
	drainSyncQueue(t, lbc)
	if manager.reloads != 0 || manager.configs != 0 {
		t.Errorf("new endpoints: %d reloads, %d configs written, want none", manager.reloads, manager.configs)
	}
	if want := []string{"10.0.0.1:8080", "10.0.0.2:8080", "10.0.0.3:8080"}; !reflect.DeepEqual(manager.servers[teaUpstream], want) {
		t.Errorf("the module has %v for tea, want %v", manager.servers[teaUpstream], want)
	}

	// the next reload has them in the file
	if err := lbc.configurator.Reload(false); err != nil {
		t.Fatal(err)
	}
	if config := manager.written["default_cafe"]; !strings.Contains(config, "server 10.0.0.3:8080") {
		t.Errorf("the config reloaded without the new servers:\n%s", config)
	}

	// the deleted EndpointSlice, as the informer hands it over when it missed the deletion: its Service still loses its servers
	manager.reset()
	lbc.endpointsTask.started = time.Time{}
	if err := sliceStore.Delete(second); err != nil {
		t.Fatal(err)
	}
	handlers.OnDelete(cache.DeletedFinalStateUnknown{Key: "default/tea-2", Obj: second})
	drainSyncQueue(t, lbc)
	if want := []string{"10.0.0.1:8080", "10.0.0.2:8080"}; manager.reloads != 0 || !reflect.DeepEqual(manager.servers[teaUpstream], want) {
		t.Errorf("a deleted EndpointSlice: %d reloads, the module has %v for tea, want %v", manager.reloads, manager.servers[teaUpstream], want)
	}

	// and the last one
	manager.reset()
	lbc.endpointsTask.started = time.Time{}
	if err := sliceStore.Delete(changed); err != nil {
		t.Fatal(err)
	}
	handlers.OnDelete(changed)
	drainSyncQueue(t, lbc)
	if manager.reloads != 0 || len(manager.servers[teaUpstream]) != 0 {
		t.Errorf("the last EndpointSlice deleted: %d reloads, the module has %v for tea, want none", manager.reloads, manager.servers[teaUpstream])
	}
}

func TestEndpointsTaskWaitsForTheInterval(t *testing.T) {
	t.Parallel()
	retryWhenSlow(t, endpointsTaskWaitsForTheInterval)
}

func endpointsTaskWaitsForTheInterval(t *testing.T) bool {
	t.Helper()
	lbc, manager, sliceStore := newEndpointsTestLBC(t)
	handlers := createEndpointSliceHandlers(lbc)

	update := func(ips ...string) {
		t.Helper()
		slice := teaSlice("tea-1", ips...)
		if err := sliceStore.Update(slice); err != nil {
			t.Fatal(err)
		}
		handlers.OnUpdate(teaSlice("tea-1"), slice)
	}

	manager.reset()
	update("10.0.0.1", "10.0.0.2")
	drainSyncQueue(t, lbc)
	if len(manager.updates) != 1 {
		t.Fatalf("the first change after a while: updates %v, want one at once", manager.updates)
	}

	// right after it, the next one waits for the interval from when the task started
	started := lbc.endpointsTask.started
	update("10.0.0.1", "10.0.0.2", "10.0.0.3")
	drainSyncQueue(t, lbc)
	if len(manager.updates) != 1 {
		if time.Since(started) >= endpointsSyncInterval {
			return false // the runner took longer than the interval: not right after
		}
		t.Fatalf("the change right after: updates %v, want it to wait", manager.updates)
	}
	obj, _ := lbc.syncQueue.queue.Get()
	if waited := time.Since(started); waited < endpointsSyncInterval {
		t.Errorf("the endpoints task came %v after it last started, want %v", waited, endpointsSyncInterval)
	}
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	drainSyncQueue(t, lbc)
	if len(manager.updates) != 2 || len(manager.servers[teaUpstream]) != 3 {
		t.Errorf("after the interval: updates %v, servers %v", manager.updates, manager.servers[teaUpstream])
	}
	return true
}

// In a batch the servers still go to the module, and no reload is made for
// them: before, they were dropped while reloads waited, and the batch ended in
// a reload because an EndpointSlice was in it.
func TestEndpointsInABatchGoToTheModuleWithoutAReload(t *testing.T) {
	t.Parallel()
	lbc, manager, sliceStore := newEndpointsTestLBC(t)
	handlers := createEndpointSliceHandlers(lbc)

	manager.reset()
	changed := teaSlice("tea-1", "10.0.0.1", "10.0.0.2")
	if err := sliceStore.Update(changed); err != nil {
		t.Fatal(err)
	}
	// events that make a batch - the Service's and an EndpointSlice that is gone -
	// and one more that comes after the endpoints task was queued, so that it runs in the batch
	handlers.OnUpdate(teaSlice("tea-1", "10.0.0.1"), changed)
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-0"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-1"})
	obj, _ := lbc.syncQueue.queue.Get()
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	if !lbc.batchSyncEnabled {
		t.Fatal("three events made no batch")
	}
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-2"})
	drainSyncQueue(t, lbc)
	if lbc.batchSyncEnabled {
		t.Fatal("the batch did not end")
	}
	if manager.reloads != 0 {
		t.Errorf("the batch ended in %d reloads, want none", manager.reloads)
	}
	if want := []string{"10.0.0.1:8080", "10.0.0.2:8080"}; !reflect.DeepEqual(manager.servers[teaUpstream], want) {
		t.Errorf("the module has %v for tea, want %v", manager.servers[teaUpstream], want)
	}
}

// A server the module refuses in a batch - an upstream the next reload
// brings - makes the batch end in a reload, with the configs written.
func TestEndpointsRefusedInABatchReload(t *testing.T) {
	t.Parallel()
	lbc, manager, sliceStore := newEndpointsTestLBC(t)
	handlers := createEndpointSliceHandlers(lbc)
	manager.refuse = true

	manager.reset()
	changed := teaSlice("tea-1", "10.0.0.1", "10.0.0.2")
	if err := sliceStore.Update(changed); err != nil {
		t.Fatal(err)
	}
	handlers.OnUpdate(teaSlice("tea-1", "10.0.0.1"), changed)
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-0"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-1"})
	obj, _ := lbc.syncQueue.queue.Get()
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-2"})
	drainSyncQueue(t, lbc)
	if manager.reloads != 1 {
		t.Errorf("a refused server: %d reloads, want 1", manager.reloads)
	}
	if !strings.Contains(manager.written["default_cafe"], "server 10.0.0.2:8080") {
		t.Errorf("the reload had the config without the new server:\n%s", manager.written["default_cafe"])
	}
}

// A batch ends in a reload only when something in it asked for one: Service
// updates that change no config, which made every batch end in a reload, do
// not.
func TestBatchEndsInAReloadOnlyWhenAConfigChanged(t *testing.T) {
	t.Parallel()
	lbc, manager, _ := newEndpointsTestLBC(t)

	manager.reset()
	lbc.syncQueue.Add(task{Kind: service, Key: "default/tea"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-0"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-1"})
	obj, _ := lbc.syncQueue.queue.Get()
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	if !lbc.batchSyncEnabled {
		t.Fatal("three events made no batch")
	}
	drainSyncQueue(t, lbc)
	if manager.reloads != 0 {
		t.Errorf("a batch that changed no config: %d reloads, want none", manager.reloads)
	}

	// a change of a config in a batch still reloads at its end
	manager.reset()
	ing := lbc.configuration.GetResources()[0].(*IngressConfiguration).Ingress.DeepCopy()
	ing.Annotations["nginx.org/proxy-read-timeout"] = "33s"
	lbc.configuration.AddOrUpdateIngress(ing)
	lbc.syncQueue.Add(task{Kind: service, Key: "default/tea"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-0"})
	lbc.syncQueue.Add(task{Kind: endpointslice, Key: "default/gone-1"})
	drainSyncQueue(t, lbc)
	if manager.reloads != 1 {
		t.Errorf("a batch that changed a config: %d reloads, want 1", manager.reloads)
	}
}

// Reloads come at most once per reload interval: a change right after a
// reload waits, and the reload task makes it with whatever came meanwhile.
func TestReloadsWaitForTheSyncRateLimit(t *testing.T) {
	t.Parallel()
	retryWhenSlow(t, reloadsWaitForTheSyncRateLimit)
}

func reloadsWaitForTheSyncRateLimit(t *testing.T) bool {
	t.Helper()
	lbc, manager, _ := newEndpointsTestLBC(t, 5) // a reload at most every 200 ms
	change := func(timeout string) {
		t.Helper()
		ing := lbc.configuration.GetResources()[0].(*IngressConfiguration).Ingress.DeepCopy()
		ing.Annotations["nginx.org/proxy-read-timeout"] = timeout
		lbc.configuration.AddOrUpdateIngress(ing)
		lbc.sync(task{Kind: service, Key: "default/tea"})
	}

	// the Ingress was just added, with a reload: the next two changes wait for the interval, together
	manager.reset()
	next := lbc.configurator.NextReload()
	change("33s")
	change("34s")
	if manager.reloads != 0 || lbc.syncQueue.queue.Len() != 0 {
		if !time.Now().Before(next) {
			return false // the runner took longer than the interval: not right after the reload
		}
		t.Fatalf("changes right after a reload: %d reloads, %d queued, want them to wait", manager.reloads, lbc.syncQueue.queue.Len())
	}
	obj, _ := lbc.syncQueue.queue.Get()
	if early := time.Until(next); early > 0 {
		t.Errorf("the reload came %v before the end of the interval", early)
	}
	lbc.sync(obj.(task))
	lbc.syncQueue.queue.Done(obj)
	if manager.reloads != 1 || !strings.Contains(manager.written["default_cafe"], "proxy_read_timeout 34s") {
		t.Errorf("after the interval: %d reloads, want 1 with the last change", manager.reloads)
	}

	// a change after a quiet time reloads at once
	time.Sleep(250 * time.Millisecond)
	manager.reset()
	change("35s")
	if manager.reloads != 1 || lbc.syncQueue.queue.Len() != 0 {
		t.Errorf("a change after a quiet time: %d reloads, %d queued, want one at once", manager.reloads, lbc.syncQueue.queue.Len())
	}
	return true
}

// A certificate the module refuses is given again, after a backoff that
// doubles while it keeps refusing.
func TestRefusedCertificatesAreGivenAgain(t *testing.T) {
	t.Parallel()
	lbc, manager, secretCache := newCertificatesTestLBC(t, 1, tlsSecret(t, "s0", "h0.example.com"))
	lbc.configuration.isDynamicUpstreams = true
	lbc.retryBackoff = 20 * time.Millisecond
	// runs the next task of the queue, which is to be of the kind given and not to come before it is due
	next := func(want kind, due time.Time) {
		t.Helper()
		obj, _ := lbc.syncQueue.queue.Get()
		if obj.(task).Kind != want {
			t.Fatalf("the task %v came, want one of kind %v", obj, want)
		}
		if early := time.Until(due); early > 0 {
			t.Errorf("the task %v came %v early", obj, early)
		}
		lbc.sync(obj.(task))
		lbc.syncQueue.queue.Done(obj)
	}

	manager.reset()
	manager.fail = 2
	changed := tlsSecret(t, "s0", "h0.example.com")
	if err := secretCache.Update(changed); err != nil {
		t.Fatal(err)
	}
	// the certificates task, one by one rather than draining the queue, which
	// would run the retry too once its timer fired on a slow runner
	scheduled := time.Now()
	lbc.sync(task{Kind: secret, Key: "default/s0"})
	next(certificates, time.Time{})
	if len(manager.set) != 0 || !lbc.retryTask.scheduled {
		t.Fatalf("a refused certificate: set %v, retry scheduled %v", manager.set, lbc.retryTask.scheduled)
	}
	for _, backoff := range []time.Duration{20 * time.Millisecond, 40 * time.Millisecond} {
		if lbc.retryBackoff != backoff {
			t.Errorf("the retry waits %v, want %v", lbc.retryBackoff, backoff)
		}
		// the retry task is due a backoff after it was scheduled, and does not come before
		if wait := lbc.retryTask.started.Sub(scheduled); wait < backoff {
			t.Errorf("the retry is due %v after it was scheduled, want %v", wait, backoff)
		}
		scheduled = time.Now()
		next(retry, lbc.retryTask.started)
	}
	if !slices.Equal(manager.set, []string{"h0.example.com"}) || manager.module["h0.example.com"] != string(changed.Data[api_v1.TLSCertKey]) {
		t.Errorf("after the retries: set %v", manager.set)
	}
	if lbc.retryTask.scheduled || lbc.retryBackoff != 0 || lbc.configurator.PushesFailed() {
		t.Errorf("after the retries: scheduled %v, backoff %v, failed %v", lbc.retryTask.scheduled, lbc.retryBackoff, lbc.configurator.PushesFailed())
	}
}
