package gateway

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes/fake"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

const ctrlNs = "wxingress"

// ownAddressController is a controller whose Gateways have addresses of
// their own, with a fake API server and a lister that follows it as an
// informer would.
type ownAddressController struct {
	*Controller
	client  *fake.Clientset
	indexer cache.Indexer
	leader  bool
}

func newOwnAddressController(t *testing.T) *ownAddressController {
	t.Helper()
	c := testController()
	c.cfg.DefaultGateway = types.NamespacedName{}
	c.cfg.Namespace = ctrlNs
	c.cfg.ExternalService = "wxi"
	c.cfg.ListenerPortMin, c.cfg.ListenerPortMax = 20000, 20009
	c.cfg.ReservedPorts = []int32{20005}
	ctrlSvc := &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: ctrlNs, Name: "wxi"},
		Spec:       v1.ServiceSpec{Selector: map[string]string{"app": "wxi"}},
	}
	o := &ownAddressController{
		Controller: c,
		client:     fake.NewClientset(ctrlSvc),
		indexer:    cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}),
		leader:     true,
	}
	_ = o.indexer.Add(ctrlSvc)
	c.cfg.KubeClient = o.client
	c.services = corelisters.NewServiceLister(o.indexer)
	// the controller's own Service has two pods
	ready := true
	slices := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_ = slices.Add(&discoveryv1.EndpointSlice{
		ObjectMeta:  metav1.ObjectMeta{Namespace: ctrlNs, Name: "wxi-abc", Labels: map[string]string{discoveryv1.LabelServiceName: "wxi"}},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{Addresses: []string{"10.0.0.2"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
			{Addresses: []string{"10.0.0.1"}, Conditions: discoveryv1.EndpointConditions{Ready: &ready}},
		},
	})
	c.endpointSlices = discoverylisters.NewEndpointSliceLister(slices)
	c.slicesWritten = map[string]string{}
	c.cfg.IsLeader = func() bool { return o.leader }
	return o
}

// sync copies the Services of the API server into the lister.
func (o *ownAddressController) sync(t *testing.T) {
	t.Helper()
	list, err := o.client.CoreV1().Services("").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range o.indexer.List() {
		_ = o.indexer.Delete(obj)
	}
	for i := range list.Items {
		_ = o.indexer.Add(&list.Items[i])
	}
}

func (o *ownAddressController) service(t *testing.T, gw string) *v1.Service {
	t.Helper()
	svc, err := o.client.CoreV1().Services(gwNs).Get(context.Background(),
		ServiceName(types.NamespacedName{Namespace: gwNs, Name: gw}, DefaultGatewayClass), metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return svc
}

func listenerOn(name string, port int32, hostname string) gatewayv1.Listener {
	l := httpListener(name, hostname)
	l.Port = gatewayv1.PortNumber(port)
	return l
}

func TestGatewaysGetAddressesOfTheirOwn(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().
		gateway("gw", listenerOn("http", 80, ""), listenerOn("alt", 8080, "alt.example.com")).
		gateway("other", listenerOn("http", 80, "")).
		service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	// the route attaches to both Gateways
	r := b.s.HTTPRoutes[types.NamespacedName{Namespace: appNs, Name: "r"}]
	r.Spec.ParentRefs = append(r.Spec.ParentRefs, gatewayv1.ParentReference{Namespace: ptr(gatewayv1.Namespace(gwNs)), Name: "other"})

	// the leader makes the Services and serves at once
	res := build(t, o.Controller, b)
	svc := o.service(t, "gw")
	if svc == nil {
		t.Fatal("no Service for gw")
	}
	if svc.Spec.Type != v1.ServiceTypeLoadBalancer || len(svc.Spec.Selector) != 0 || len(svc.Spec.Ports) != 2 {
		t.Errorf("Service %+v", svc.Spec)
	}
	if len(svc.OwnerReferences) != 1 || svc.OwnerReferences[0].Kind != "Gateway" || svc.OwnerReferences[0].Name != "gw" {
		t.Errorf("owners %+v", svc.OwnerReferences)
	}
	slice, err := o.client.DiscoveryV1().EndpointSlices(gwNs).Get(context.Background(), svc.Name+"-ipv4", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("no EndpointSlice: %v", err)
	}
	if slice.Labels[discoveryv1.LabelServiceName] != svc.Name || len(slice.Endpoints) != 2 || slice.Endpoints[0].Addresses[0] != "10.0.0.1" || len(slice.Ports) != 2 {
		t.Errorf("EndpointSlice %+v", slice)
	}
	if svc.Labels[LabelGatewayName] != "gw" || svc.Annotations[AnnotationGateway] != gwNs+"/gw" {
		t.Errorf("labels %v annotations %v", svc.Labels, svc.Annotations)
	}
	ports := nginxPortsOf(svc)
	p80, p8080 := ports[listenerPort{80, v1.ProtocolTCP}], ports[listenerPort{8080, v1.ProtocolTCP}]
	if p80 < 20000 || p80 > 20009 || p8080 < 20000 || p8080 > 20009 || p80 == p8080 || p80 == 20005 || p8080 == 20005 {
		t.Errorf("nginx ports %v", ports)
	}
	other := nginxPortsOf(o.service(t, "other"))[listenerPort{80, v1.ProtocolTCP}]
	if other == 0 || other == p80 || other == p8080 {
		t.Errorf("other's nginx port %d collides with gw's %v", other, ports)
	}

	// a catch-all VirtualServer per Gateway, on its own listener, and one for alt
	listeners := map[string]int{}
	for _, l := range res.Listeners {
		listeners[l.Name] = l.Port
	}
	hosts := map[string]int{}
	for _, vs := range res.VirtualServers {
		if vs.Spec.Listener == nil || vs.Spec.Listener.HTTP == "" {
			t.Fatalf("VirtualServer %s has no listener", vs.Name)
		}
		hosts[vs.Annotations[AnnotationGateway]+" "+vs.Spec.Host] = listeners[vs.Spec.Listener.HTTP]
	}
	want := map[string]int{
		gwNs + "/gw ~^":              int(p80),
		gwNs + "/gw alt.example.com": int(p8080),
		gwNs + "/other ~^":           int(other),
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts %v, want %v", hosts, want)
	}
	for k, v := range want {
		if hosts[k] != v {
			t.Errorf("%s on %d, want %d", k, hosts[k], v)
		}
	}

	// the Gateways report the addresses of their Services; the fake API
	// server assigns none, so they wait for one
	if _, ok := res.Addresses[types.NamespacedName{Namespace: gwNs, Name: "gw"}]; !ok {
		t.Error("gw has no addresses of its own")
	}

	// a replica that is not the leader serves what the Services say
	o.sync(t)
	o.leader = false
	follower := build(t, o.Controller, b)
	if len(follower.VirtualServers) != 3 || len(follower.Listeners) != 3 {
		t.Errorf("the follower serves %d VirtualServers on %d listeners", len(follower.VirtualServers), len(follower.Listeners))
	}

	// the ports stay where they are when a listener goes
	o.leader = true
	b.s.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}].Spec.Listeners = []gatewayv1.Listener{listenerOn("http", 80, "")}
	build(t, o.Controller, b)
	again := nginxPortsOf(o.service(t, "gw"))
	if len(again) != 1 || again[listenerPort{80, v1.ProtocolTCP}] != p80 {
		t.Errorf("after the listener went: %v, want 80 on %d alone", again, p80)
	}

	// a Gateway that goes loses its Service
	o.sync(t)
	delete(b.s.Gateways, types.NamespacedName{Namespace: gwNs, Name: "other"})
	build(t, o.Controller, b)
	if o.service(t, "other") != nil {
		t.Error("the Service of a Gateway that is gone is still there")
	}
	if o.service(t, "gw") == nil {
		t.Error("the Service of a Gateway that is there went")
	}
}

func TestFollowerWithoutServiceServesNothing(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	o.leader = false
	b := newState().gateway("gw", listenerOn("http", 80, "")).service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	res := build(t, o.Controller, b)
	if len(res.VirtualServers) != 0 || len(res.Listeners) != 0 {
		t.Errorf("served %d VirtualServers without a Service", len(res.VirtualServers))
	}
	gw := res.Graph.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}]
	found := false
	for _, c := range gw.Conditions {
		if c.Type == string(gatewayv1.GatewayConditionProgrammed) && c.Status == metav1.ConditionFalse &&
			c.Reason == string(gatewayv1.GatewayReasonAddressNotAssigned) {
			found = true
		}
	}
	if !found {
		t.Errorf("conditions %+v, want Programmed=False AddressNotAssigned", gw.Conditions)
	}
}

func TestDefaultGatewaySharesTheDefaultPorts(t *testing.T) {
	t.Parallel()
	c := testController() // its default Gateway is infra/gw
	b := newState().
		gateway("gw", listenerOn("http", 80, ""), listenerOn("alt", 8080, "")).
		service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	res := build(t, c, b)
	vs := onlyVS(t, res)
	if vs.Spec.Listener != nil || len(res.Listeners) != 0 {
		t.Errorf("the default Gateway has listeners: %+v %+v", vs.Spec.Listener, res.Listeners)
	}
	if _, ok := res.Addresses[types.NamespacedName{Namespace: gwNs, Name: "gw"}]; ok {
		t.Error("the default Gateway has addresses of its own")
	}
	var alt *gatewayv1.Listener
	for _, l := range res.Graph.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}].Listeners {
		if l.Name == "alt" {
			if l.Valid {
				t.Error("the listener on 8080 of the default Gateway is valid")
			}
			for _, cond := range l.Conditions {
				if cond.Type == string(gatewayv1.ListenerConditionAccepted) && cond.Reason == string(gatewayv1.ListenerReasonPortUnavailable) {
					alt = &l.Source
				}
			}
		}
	}
	if alt == nil {
		t.Error("the listener on 8080 of the default Gateway is not PortUnavailable")
	}
}

func TestServiceName(t *testing.T) {
	t.Parallel()
	for _, gw := range []types.NamespacedName{
		{Namespace: "infra", Name: "gw"},
		{Namespace: "infra", Name: "gw.with.dots"},
		{Namespace: "a-very-long-namespace-name-that-goes-on", Name: "and-a-very-long-gateway-name-as-well"},
		{Namespace: "1numeric", Name: "gw"},
	} {
		n := ServiceName(gw, "wxingress")
		if errs := validation.IsDNS1035Label(n); len(errs) != 0 {
			t.Errorf("%v: %q is no Service name: %v", gw, n, errs)
		}
	}
	if n := ServiceName(types.NamespacedName{Namespace: "infra", Name: "gw"}, "wxingress"); n != "gw-wxingress" {
		t.Errorf("name %q", n)
	}
}

func TestListenerNamesAreNoGlobalConfigurationNames(t *testing.T) {
	t.Parallel()
	n := ListenerName(types.NamespacedName{Namespace: "a", Name: "b"}, 80)
	if !IsGatewayListener(n) {
		t.Errorf("%q is not marked", n)
	}
	_ = conf_v1.Listener{Name: n}
}

// TestServicesLeftInANamespaceNoLongerWatchedGo: a namespace that loses its
// label takes its Gateways out of the graph, and the leader deletes their
// Services - but not while a namespace that got the label has not listed its
// Gateways yet.
func TestServicesLeftInANamespaceNoLongerWatchedGo(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().gateway("gw", listenerOn("http", 80, "")).gateway("other", listenerOn("http", 80, ""))
	build(t, o.Controller, b)
	o.sync(t)
	if o.service(t, "other") == nil {
		t.Fatal("no Service for other")
	}

	// gwNs is watched again, its informers have not listed its Gateways yet:
	// the graph has neither, and both Services stay
	o.cfg.GatewayClient = gatewayClient(t)
	w := o.newNSWatch(gwNs)
	o.watches.byNamespace = map[string]*nsWatch{gwNs: w}
	empty := newState()
	build(t, o.Controller, empty)
	if o.service(t, "gw") == nil || o.service(t, "other") == nil {
		t.Fatal("a Service went while its namespace's Gateways were not listed yet")
	}

	// once they are, the Services of Gateways that are not there go
	stop := make(chan struct{})
	defer close(stop)
	w.factory.Start(stop)
	for _, inf := range w.informers {
		if !cache.WaitForCacheSync(stop, inf.HasSynced) {
			t.Fatal("no sync")
		}
	}
	build(t, o.Controller, newState().gateway("gw", listenerOn("http", 80, "")))
	if o.service(t, "other") != nil {
		t.Error("the Service of a Gateway no longer listed is still there")
	}
	if o.service(t, "gw") == nil {
		t.Error("the Service of a Gateway that is there went")
	}

	// the namespace loses its label: not watched, its Services go
	o.sync(t)
	o.watches.byNamespace = map[string]*nsWatch{}
	build(t, o.Controller, empty)
	if o.service(t, "gw") != nil {
		t.Error("the Service of a Gateway in a namespace no longer watched is still there")
	}
}
