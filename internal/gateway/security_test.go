package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

// setTargetPort changes, as the owners of the Gateway's namespace can, the
// nginx port of a listener port in the Service of gw on the API server.
func (o *ownAddressController) setTargetPort(t *testing.T, gw string, port, target int32) {
	t.Helper()
	svc := o.service(t, gw)
	for i := range svc.Spec.Ports {
		if svc.Spec.Ports[i].Port == port {
			svc.Spec.Ports[i].TargetPort = intstr.FromInt32(target)
		}
	}
	if _, err := o.client.CoreV1().Services(gwNs).Update(context.Background(), svc, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// setResourceVersion gives the Service of gw in the lister a resourceVersion,
// which the fake API server does not count up.
func (o *ownAddressController) setResourceVersion(t *testing.T, gw, rv string) {
	t.Helper()
	key := gwNs + "/" + ServiceName(types.NamespacedName{Namespace: gwNs, Name: gw}, DefaultGatewayClass)
	obj, ok, err := o.indexer.GetByKey(key)
	if err != nil || !ok {
		t.Fatalf("no Service %s in the lister", key)
	}
	svc := obj.(*v1.Service).DeepCopy()
	svc.ResourceVersion = rv
	_ = o.indexer.Update(svc)
}

// listenerPorts are the nginx ports of the listeners of a Build, by name.
func listenerPorts(res Result) map[string]int {
	out := map[string]int{}
	for _, l := range res.Listeners {
		out[l.Name] = l.Port
	}
	return out
}

func nginxPortOf(svc *v1.Service, port int32) int32 {
	return nginxPortsOf(svc)[listenerPort{port: port, protocol: v1.ProtocolTCP}]
}

// TestATenantCannotTakeTheNginxPortOfAnotherGateway: the owners of a
// Gateway's namespace can change its Service, the record of the nginx port
// of each listener. A port of another Gateway they set there is not served,
// and the leader gives the listener a free one again.
func TestATenantCannotTakeTheNginxPortOfAnotherGateway(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	// "gw" sorts before "victim": the order of the names does not decide
	b := newState().
		gateway("gw", listenerOn("http", 80, "")).
		gateway("victim", listenerOn("http", 80, "")).
		service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	r := b.s.HTTPRoutes[routeKey("r")]
	r.Spec.ParentRefs = append(r.Spec.ParentRefs, gatewayv1.ParentReference{Namespace: ptr(gatewayv1.Namespace(gwNs)), Name: "victim"})
	build(t, o.Controller, b)
	victimPort := nginxPortOf(o.service(t, "victim"), 80)
	ownPort := nginxPortOf(o.service(t, "gw"), 80)
	if victimPort == 0 || ownPort == 0 || victimPort == ownPort {
		t.Fatalf("ports %d and %d", victimPort, ownPort)
	}

	o.setTargetPort(t, "gw", 80, victimPort)
	o.sync(t)
	o.setResourceVersion(t, "victim", "100")
	o.setResourceVersion(t, "gw", "200")

	// a replica that is not the leader serves the port to the victim alone
	o.leader = false
	res := build(t, o.Controller, b)
	ports := listenerPorts(res)
	if got := ports[ListenerName(gwKey("victim"), 80)]; got != int(victimPort) {
		t.Errorf("the victim's listener is on %d, want %d", got, victimPort)
	}
	if got, ok := ports[ListenerName(gwKey("gw"), 80)]; ok {
		t.Errorf("the listener of the Gateway whose Service was changed is served on %d", got)
	}

	// the leader puts the changed Service right, on a free port
	o.leader = true
	build(t, o.Controller, b)
	fixed := nginxPortOf(o.service(t, "gw"), 80)
	if fixed == victimPort || fixed < 20000 || fixed > 20009 || fixed == 20005 {
		t.Errorf("the changed Service has %d, the victim %d", fixed, victimPort)
	}
	if nginxPortOf(o.service(t, "victim"), 80) != victimPort {
		t.Error("the victim's port changed")
	}
}

// TestNginxPortsOutsideTheRangeAreNotServed: a port outside
// -gateway-listener-ports - 80, which nginx listens on for Ingresses, so that
// a TCP listener there would fail every reload, or any other - is not
// served, and the leader puts a port of the range in its place.
func TestNginxPortsOutsideTheRangeAreNotServed(t *testing.T) {
	t.Parallel()
	for _, target := range []int32{80, 443, 8081, 10254, 20005, 30000, 19999} {
		o := newOwnAddressController(t)
		b := newState().gateway("gw", l4Listener("tcp", 5432, gatewayv1.TCPProtocolType, "")).service("db", 5432).
			tcpRoute("db", parentRef("gw", "tcp"), backendRef("db", 5432, 1))
		build(t, o.Controller, b)
		o.setTargetPort(t, "gw", 5432, target)
		o.sync(t)

		o.leader = false
		res := build(t, o.Controller, b)
		for _, l := range res.Listeners {
			if l.Port == int(target) {
				t.Errorf("target %d: listener %s is served on it", target, l.Name)
			}
		}
		if len(res.Listeners) != 0 {
			t.Errorf("target %d: listeners %+v", target, res.Listeners)
		}

		o.leader = true
		build(t, o.Controller, b)
		if got := nginxPortOf(o.service(t, "gw"), 5432); got < 20000 || got > 20009 || got == 20005 {
			t.Errorf("target %d: the leader left %d", target, got)
		}
	}
}

// TestOneNginxPortForTwoListenersOfAService: the same nginx port for two
// listener ports of one Service - HTTP and HTTPS on one nginx port would
// fail the reload - is kept by one of them.
func TestOneNginxPortForTwoListenersOfAService(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().gateway("gw", listenerOn("http", 80, ""), listenerOn("alt", 8080, "")).service("a", 80).
		route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	build(t, o.Controller, b)
	p80 := nginxPortOf(o.service(t, "gw"), 80)
	o.setTargetPort(t, "gw", 8080, p80)
	o.sync(t)
	o.leader = false
	seen := map[int]bool{}
	served := build(t, o.Controller, b).Listeners
	if len(served) != 1 {
		t.Errorf("served %+v, want one listener", served)
	}
	for _, l := range served {
		if seen[l.Port] {
			t.Errorf("nginx port %d served twice", l.Port)
		}
		seen[l.Port] = true
	}
	o.leader = true
	build(t, o.Controller, b)
	svc := o.service(t, "gw")
	if a, c := nginxPortOf(svc, 80), nginxPortOf(svc, 8080); a == c || a == 0 || c == 0 {
		t.Errorf("after the leader: %d and %d", a, c)
	}
}

// TestAServiceTheControllerDidNotMakeIsNotTakenOver: a Service with the name
// and labels of a Gateway's, made by someone else, or owned by another
// object or by a Gateway of the name before, is neither used, nor changed,
// nor deleted.
func TestAServiceTheControllerDidNotMakeIsNotTakenOver(t *testing.T) {
	t.Parallel()
	for name, owners := range map[string][]metav1.OwnerReference{
		"no owner":          nil,
		"another Gateway":   {{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Name: "victim", UID: "uid-gw", Controller: ptr(true)}},
		"an old UID":        {{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Name: "gw", UID: "uid-old", Controller: ptr(true)}},
		"not as controller": {{APIVersion: gatewayv1.GroupVersion.String(), Kind: "Gateway", Name: "gw", UID: "uid-gw"}},
		"another kind":      {{APIVersion: "v1", Kind: "ConfigMap", Name: "gw", UID: "uid-gw", Controller: ptr(true)}},
	} {
		o := newOwnAddressController(t)
		svcName := ServiceName(gwKey("gw"), DefaultGatewayClass)
		premade := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: gwNs, Name: svcName, OwnerReferences: owners,
				Labels: map[string]string{LabelManagedBy: managedBy, LabelGatewayClass: DefaultGatewayClass, LabelGatewayName: "gw"},
			},
			Spec: v1.ServiceSpec{Ports: []v1.ServicePort{{Name: "tcp-80", Protocol: v1.ProtocolTCP, Port: 80, TargetPort: intstr.FromInt32(20001)}}},
		}
		if _, err := o.client.CoreV1().Services(gwNs).Create(context.Background(), premade, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		o.sync(t)
		b := newState().gateway("gw", listenerOn("http", 80, "")).service("a", 80).
			route("r", nil, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
		b.s.Gateways[gwKey("gw")].UID = "uid-gw"

		res := build(t, o.Controller, b)
		if len(res.Listeners) != 0 || len(res.VirtualServers) != 0 {
			t.Errorf("%s: served %+v", name, res.Listeners)
		}
		svc := o.service(t, "gw")
		if svc == nil {
			t.Fatalf("%s: the Service was deleted", name)
		}
		if len(svc.OwnerReferences) != len(owners) || svc.Annotations[AnnotationGateway] != "" {
			t.Errorf("%s: the Service was taken over: %+v %v", name, svc.OwnerReferences, svc.Annotations)
		}
		pending := false
		for _, c := range res.Graph.Gateways[gwKey("gw")].Conditions {
			if c.Reason == string(gatewayv1.GatewayReasonAddressNotAssigned) && strings.Contains(c.Message, "not made for it") {
				pending = true
			}
		}
		if !pending {
			t.Errorf("%s: the Gateway does not say why it has no address", name)
		}
	}
}

// TestRouteVirtualServersCompeteWithTheirRoutesTime: of a host an Ingress or
// VirtualServer has as well the oldest gets it. A VirtualServer of a Gateway
// competes with the time of the first route that brought the hostname, not
// before the Gateway was made - not with the Gateway's time alone, which
// would let a new route of any namespace take the host of an older Ingress.
func TestRouteVirtualServersCompeteWithTheirRoutesTime(t *testing.T) {
	t.Parallel()
	t0 := metav1.NewTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
	t1 := metav1.NewTime(t0.Add(time.Hour))
	t2 := metav1.NewTime(t0.Add(2 * time.Hour))
	t3 := metav1.NewTime(t0.Add(3 * time.Hour))

	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("new", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	b.s.Gateways[gwKey("gw")].CreationTimestamp = t0
	b.s.HTTPRoutes[routeKey("new")].CreationTimestamp = t2
	vs := onlyVS(t, build(t, testController(), b))
	if !vs.CreationTimestamp.Equal(&t2) {
		t.Errorf("created %v, want the route's %v", vs.CreationTimestamp, t2)
	}

	// a second, newer route for the host does not make it younger; an older
	// one makes it as old as that one
	b.route("newer", []string{"x.example.com"}, filterRoute("/newer"))
	b.s.HTTPRoutes[routeKey("newer")].CreationTimestamp = t3
	vs = onlyVS(t, build(t, testController(), b))
	if !vs.CreationTimestamp.Equal(&t2) {
		t.Errorf("with a newer route: created %v, want %v", vs.CreationTimestamp, t2)
	}
	b.route("older", []string{"x.example.com"}, filterRoute("/older"))
	b.s.HTTPRoutes[routeKey("older")].CreationTimestamp = t1
	vs = onlyVS(t, build(t, testController(), b))
	if !vs.CreationTimestamp.Equal(&t1) {
		t.Errorf("with an older route: created %v, want %v", vs.CreationTimestamp, t1)
	}

	// a route made before the Gateway competes with the Gateway's time
	b.s.Gateways[gwKey("gw")].CreationTimestamp = t3
	vs = onlyVS(t, build(t, testController(), b))
	if !vs.CreationTimestamp.Equal(&t3) {
		t.Errorf("with a newer Gateway: created %v, want %v", vs.CreationTimestamp, t3)
	}
}

// TestListenersThatVerifyClientCertificatesAreNotServed: a VirtualServer or
// TransportServer made from a Gateway does not carry the verification of
// client certificates; a listener that asks for it is not accepted rather
// than served to any client.
func TestListenersThatVerifyClientCertificatesAreNotServed(t *testing.T) {
	t.Parallel()
	gw := &graph.Gateway{Listeners: []*graph.Listener{
		{Name: "mtls", Valid: true, CACertificateRefs: []gatewayv1.ObjectReference{{Kind: "ConfigMap", Name: "ca"}}},
		{Name: "fallback", Valid: true, ValidationMode: gatewayv1.AllowInsecureFallback},
		{Name: "plain", Valid: true},
	}}
	restrictFrontendValidation(gw)
	for _, l := range gw.Listeners {
		rejected := false
		for _, c := range l.Conditions {
			if c.Type == string(gatewayv1.ListenerConditionAccepted) && c.Status == metav1.ConditionFalse &&
				c.Reason == string(gatewayv1.ListenerReasonUnsupportedValue) {
				rejected = true
			}
		}
		if want := l.Name != "plain"; l.Valid == want || rejected != want {
			t.Errorf("%s: valid %v, rejected %v", l.Name, l.Valid, rejected)
		}
	}

	// and should a server with the verification get through, it is not served
	var out mapped
	vs := newVirtualServer(&graph.Gateway{Source: &gatewayv1.Gateway{}}, "GW-x", "x.example.com", mapOptions{}, gatewayPolicies{})
	s := &dataplane.VirtualServer{Hostname: "x.example.com", SSL: &dataplane.SSL{
		KeyPairIDs: []dataplane.SSLKeyPairID{"ssl_keypair_infra_cert"}, VerifyClient: dataplane.SSLVerifyClientOn, ClientCertBundleID: "ca",
	}}
	if out.setTLS(vs, gwKey("gw"), s) {
		t.Error("a server that verifies client certificates is served")
	}

	// through the graph: a Gateway whose HTTPS listener verifies client
	// certificates serves nothing on it
	b := newState().
		gateway("gw", httpsListener("https", 443, "x.example.com", "", "cert")).
		secret(tlsSecret(t, gwNs, "cert", "x.example.com")).
		service("a", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	ca := tlsSecret(t, gwNs, "ca", "ca.example.com")
	b.s.ConfigMaps = map[types.NamespacedName]*v1.ConfigMap{{Namespace: gwNs, Name: "ca"}: {
		ObjectMeta: metav1.ObjectMeta{Namespace: gwNs, Name: "ca"},
		Data:       map[string]string{"ca.crt": string(ca.Data[v1.TLSCertKey])},
	}}
	b.s.Gateways[gwKey("gw")].Spec.TLS = &gatewayv1.GatewayTLSConfig{Frontend: &gatewayv1.FrontendTLSConfig{
		Default: gatewayv1.TLSConfig{Validation: &gatewayv1.FrontendTLSValidation{
			CACertificateRefs: []gatewayv1.ObjectReference{{Kind: "ConfigMap", Name: "ca"}},
		}},
	}}
	res := build(t, testController(), b)
	for _, l := range res.Graph.Gateways[gwKey("gw")].Listeners {
		if l.Valid || len(l.Conditions) == 0 || l.Conditions[0].Reason != string(gatewayv1.ListenerReasonUnsupportedValue) {
			t.Errorf("listener %s: valid %v, conditions %+v", l.Name, l.Valid, l.Conditions)
		}
	}
	for _, vs := range res.VirtualServers {
		if vs.Spec.TLS != nil {
			t.Errorf("served with TLS but without client verification: %s %s", vs.Name, vs.Spec.Host)
		}
	}
}

// TestRestrictiveAnnotationsOfARouteFailClosed: an ingress-nginx annotation
// that restricts who gets an answer, which a route does not take, makes its
// rules answer 503 rather than serve them to anyone; it is warned about once.
func TestRestrictiveAnnotationsOfARouteFailClosed(t *testing.T) {
	t.Parallel()
	for _, annotation := range []string{"auth-url", "auth-type", "whitelist-source-range", "denylist-source-range", "satisfy", "enable-modsecurity"} {
		c := testController()
		b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
			route("guarded", []string{"x.example.com"}, filterRoute("/guarded")).
			route("open", []string{"x.example.com"}, filterRoute("/open"))
		b.s.HTTPRoutes[routeKey("guarded")].Annotations = map[string]string{"nginx.ingress.kubernetes.io/" + annotation: "true"}
		res := build(t, c, b)
		vs := onlyVS(t, res)
		for _, r := range vs.Spec.Routes {
			switch r.Path {
			case "/guarded/", "=/guarded":
				if r.Action == nil || r.Action.Return == nil || r.Action.Return.Code != 503 {
					t.Errorf("%s: %s answers %+v", annotation, r.Path, r.Action)
				}
			case "/open/", "=/open":
				if r.Action == nil || r.Action.Proxy == nil {
					t.Errorf("%s: %s answers %+v", annotation, r.Path, r.Action)
				}
			}
		}
		if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], annotation) {
			t.Errorf("%s: warnings %q", annotation, res.Warnings)
		}
		if again := build(t, c, b); len(again.Warnings) != 0 {
			t.Errorf("%s: warned again %q", annotation, again.Warnings)
		}
	}

	// turning a restriction off restricts nothing
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80).
		route("r", []string{"x.example.com"}, filterRoute("/r"))
	b.s.HTTPRoutes[routeKey("r")].Annotations = map[string]string{"nginx.ingress.kubernetes.io/enable-modsecurity": "false"}
	for _, r := range onlyVS(t, build(t, testController(), b)).Spec.Routes {
		if r.Action == nil || r.Action.Proxy == nil {
			t.Errorf("enable-modsecurity false: %s answers %+v", r.Path, r.Action)
		}
	}
}
