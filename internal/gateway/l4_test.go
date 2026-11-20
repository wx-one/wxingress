package gateway

import (
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	"github.com/wx-one/wxingress/pkg/apis/configuration/validation"

	"github.com/wx-one/wxingress/internal/gateway/extras"
)

func l4Listener(name string, port int32, proto gatewayv1.ProtocolType, hostname string) gatewayv1.Listener {
	l := gatewayv1.Listener{
		Name:          gatewayv1.SectionName(name),
		Port:          gatewayv1.PortNumber(port),
		Protocol:      proto,
		AllowedRoutes: &gatewayv1.AllowedRoutes{Namespaces: &gatewayv1.RouteNamespaces{From: ptr(gatewayv1.NamespacesFromAll)}},
	}
	if hostname != "" {
		l.Hostname = ptr(gatewayv1.Hostname(hostname))
	}
	if proto == gatewayv1.TLSProtocolType {
		l.TLS = &gatewayv1.ListenerTLSConfig{Mode: ptr(gatewayv1.TLSModePassthrough)}
	}
	return l
}

func parentRef(gw, section string) gatewayv1.ParentReference {
	return gatewayv1.ParentReference{Namespace: ptr(gatewayv1.Namespace(gwNs)), Name: gatewayv1.ObjectName(gw), SectionName: ptr(gatewayv1.SectionName(section))}
}

func backendRef(name string, port, weight int32) gatewayv1.BackendRef {
	return gatewayv1.BackendRef{
		BackendObjectReference: gatewayv1.BackendObjectReference{Name: gatewayv1.ObjectName(name), Port: ptr(gatewayv1.PortNumber(port))},
		Weight:                 ptr(weight),
	}
}

func (b *stateBuilder) tlsRoute(name string, hostnames []string, parent gatewayv1.ParentReference, backend gatewayv1.BackendRef) *stateBuilder {
	if b.s.TLSRoutes == nil {
		b.s.TLSRoutes = map[types.NamespacedName]*gatewayv1.TLSRoute{}
	}
	r := &gatewayv1.TLSRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name, Generation: 1},
		Spec: gatewayv1.TLSRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Rules:           []gatewayv1.TLSRouteRule{{BackendRefs: []gatewayv1.BackendRef{backend}}},
		},
	}
	for _, h := range hostnames {
		r.Spec.Hostnames = append(r.Spec.Hostnames, gatewayv1.Hostname(h))
	}
	b.s.TLSRoutes[types.NamespacedName{Namespace: appNs, Name: name}] = r
	return b
}

func (b *stateBuilder) tcpRoute(name string, parent gatewayv1.ParentReference, backends ...gatewayv1.BackendRef) *stateBuilder {
	if b.s.TCPRoutes == nil {
		b.s.TCPRoutes = map[types.NamespacedName]*gatewayv1.TCPRoute{}
	}
	b.s.TCPRoutes[types.NamespacedName{Namespace: appNs, Name: name}] = &gatewayv1.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name, Generation: 1},
		Spec: gatewayv1.TCPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Rules:           []gatewayv1.TCPRouteRule{{BackendRefs: backends}},
		},
	}
	return b
}

func (b *stateBuilder) udpRoute(name string, parent gatewayv1.ParentReference, backends ...gatewayv1.BackendRef) *stateBuilder {
	if b.s.UDPRoutes == nil {
		b.s.UDPRoutes = map[types.NamespacedName]*gatewayv1.UDPRoute{}
	}
	b.s.UDPRoutes[types.NamespacedName{Namespace: appNs, Name: name}] = &gatewayv1.UDPRoute{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name, Generation: 1},
		Spec: gatewayv1.UDPRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{parent}},
			Rules:           []gatewayv1.UDPRouteRule{{BackendRefs: backends}},
		},
	}
	return b
}

// validTransportServers checks every TransportServer as the configuration does.
func validTransportServers(t *testing.T, res Result) map[string]*conf_v1.TransportServer {
	t.Helper()
	tsv := validation.NewTransportServerValidator(false, false, false)
	out := map[string]*conf_v1.TransportServer{}
	for _, ts := range res.TransportServers {
		if err := tsv.ValidateTransportServer(TransportServerForValidation(ts)); err != nil {
			t.Errorf("TransportServer %s is invalid: %v", ts.Name, err)
		}
		if !IsGatewayTransportServer(ts) {
			t.Errorf("TransportServer %s is not marked as made from a Gateway", ts.Name)
		}
		out[ts.Spec.Listener.Protocol+" "+ts.Annotations[AnnotationPort]+" "+ts.Spec.Host] = ts
	}
	return out
}

func TestTLSPassthrough(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().gateway("gw", l4Listener("tls", 443, gatewayv1.TLSProtocolType, "")).service("a", 443).
		tlsRoute("r", []string{"a.example.com"}, parentRef("gw", "tls"), backendRef("a", 443, 1))
	res := build(t, o.Controller, b)
	tses := validTransportServers(t, res)
	if len(tses) != 2 {
		t.Fatalf("TransportServers %v, want one for the hostname and the port's default", tses)
	}
	host, def := tses["TCP 443 a.example.com"], tses["TCP 443 "]
	if host == nil || def == nil {
		t.Fatalf("TransportServers %v", tses)
	}
	if host.Namespace != appNs || len(host.Spec.Upstreams) != 1 || host.Spec.Upstreams[0].Service != "a" || host.Spec.Upstreams[0].Port != 443 {
		t.Errorf("the hostname's %s/%s %+v", host.Namespace, host.Name, host.Spec.Upstreams)
	}
	e, _ := extras.Decode(host.Annotations[extras.Annotation])
	if len(e.StreamServer) != 1 || e.StreamServer[0] != "ssl_preread on;" || e.DefaultServer {
		t.Errorf("the hostname's extras %+v", e)
	}
	e, _ = extras.Decode(def.Annotations[extras.Annotation])
	if !e.DefaultServer || e.ProxyPass != closedSocket {
		t.Errorf("the default's extras %+v", e)
	}
	if host.Spec.Listener.Name != def.Spec.Listener.Name || host.Spec.Listener.Protocol != "TCP" {
		t.Errorf("listeners %+v %+v", host.Spec.Listener, def.Spec.Listener)
	}
	found := false
	for _, l := range res.Listeners {
		if l.Name == host.Spec.Listener.Name && l.Protocol == "TCP" && l.Port >= 20000 && !l.Ssl {
			found = true
		}
	}
	if !found {
		t.Errorf("no stream listener for it in %+v", res.Listeners)
	}
}

func TestTCPWeightsAndUDP(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().
		gateway("gw", l4Listener("tcp", 5000, gatewayv1.TCPProtocolType, ""), l4Listener("udp", 5000, gatewayv1.UDPProtocolType, "")).
		service("a", 5000).service("b", 5000).
		tcpRoute("t", parentRef("gw", "tcp"), backendRef("a", 5000, 3), backendRef("b", 5000, 1)).
		udpRoute("u", parentRef("gw", "udp"), backendRef("a", 5000, 1))
	b.s.Services[serviceKey("a")].Spec.Ports[0].Protocol = "UDP"
	res := build(t, o.Controller, b)
	tses := validTransportServers(t, res)
	tcp, udp := tses["TCP 5000 "], tses["UDP 5000 "]
	if tcp == nil || udp == nil {
		t.Fatalf("TransportServers %v", tses)
	}
	if tcp.Spec.Listener.Name == udp.Spec.Listener.Name {
		t.Errorf("TCP and UDP of one port share the listener %s", tcp.Spec.Listener.Name)
	}
	e, _ := extras.Decode(tcp.Annotations[extras.Annotation])
	if len(e.Stream) != 1 || !strings.Contains(e.Stream[0], "split_clients") || !strings.Contains(e.Stream[0], "75% {{upstream:") ||
		!strings.HasPrefix(e.ProxyPass, "$wxgw_") {
		t.Errorf("the weights %+v", e)
	}
	if _, ok := udp.Annotations[extras.Annotation]; ok {
		t.Errorf("a single backend has extras: %s", udp.Annotations[extras.Annotation])
	}
	ports := nginxPortsOf(o.service(t, "gw"))
	if len(ports) != 2 || ports[listenerPort{5000, "TCP"}] == 0 || ports[listenerPort{5000, "UDP"}] == 0 {
		t.Errorf("the Service's ports %v", ports)
	}
}

func TestGRPCRoute(t *testing.T) {
	t.Parallel()
	b := newState().gateway("gw", httpListener("http", "")).service("a", 80)
	b.s.GRPCRoutes = map[types.NamespacedName]*gatewayv1.GRPCRoute{{Namespace: appNs, Name: "g"}: {
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: "g", Generation: 1},
		Spec: gatewayv1.GRPCRouteSpec{
			CommonRouteSpec: gatewayv1.CommonRouteSpec{ParentRefs: []gatewayv1.ParentReference{{Namespace: ptr(gatewayv1.Namespace(gwNs)), Name: "gw"}}},
			Hostnames:       []gatewayv1.Hostname{"grpc.example.com"},
			Rules: []gatewayv1.GRPCRouteRule{{
				Matches: []gatewayv1.GRPCRouteMatch{{Method: &gatewayv1.GRPCMethodMatch{
					Type: ptr(gatewayv1.GRPCMethodMatchExact), Service: ptr("echo.Echo"), Method: ptr("Say"),
				}}},
				BackendRefs: []gatewayv1.GRPCBackendRef{{BackendRef: backendRef("a", 80, 1)}},
			}},
		},
	}}
	vs := onlyVS(t, build(t, testController(), b))
	if vs.Spec.HTTP2 == nil || !*vs.Spec.HTTP2 {
		t.Error("a VirtualServer with a gRPC route has no HTTP/2")
	}
	if len(vs.Spec.Upstreams) != 1 || vs.Spec.Upstreams[0].Type != "grpc" {
		t.Errorf("upstreams %+v", vs.Spec.Upstreams)
	}
	if len(vs.Spec.Routes) != 1 || vs.Spec.Routes[0].Path != "=/echo.Echo/Say" {
		t.Errorf("routes %+v", vs.Spec.Routes)
	}
}

func TestTLSTerminateAndMixedModes(t *testing.T) {
	t.Parallel()
	terminate := l4Listener("terminate", 8443, gatewayv1.TLSProtocolType, "t.example.com")
	terminate.TLS = &gatewayv1.ListenerTLSConfig{
		Mode:            ptr(gatewayv1.TLSModeTerminate),
		CertificateRefs: []gatewayv1.SecretObjectReference{{Name: "cert", Namespace: ptr(gatewayv1.Namespace(appNs))}},
	}
	o := newOwnAddressController(t)
	b := newState().gateway("gw", terminate).service("a", 443).secret(tlsSecret(t, appNs, "cert", "t.example.com")).
		tlsRoute("r", []string{"t.example.com"}, parentRef("gw", "terminate"), backendRef("a", 443, 1))
	b.s.ReferenceGrants[types.NamespacedName{Namespace: appNs, Name: "certs"}] = &gatewayv1.ReferenceGrant{
		ObjectMeta: metaOf(appNs, "certs"),
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: gwNs}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret"}},
		},
	}
	tses := validTransportServers(t, build(t, o.Controller, b))
	host, def := tses["TCP 8443 t.example.com"], tses["TCP 8443 "]
	if host == nil || def == nil {
		t.Fatalf("TransportServers %v", tses)
	}
	if host.Spec.TLS == nil || host.Spec.TLS.Secret != "cert" || host.Namespace != appNs {
		t.Errorf("the hostname's %s %+v", host.Namespace, host.Spec.TLS)
	}
	if def.Spec.TLS == nil || def.Spec.TLS.Secret != "cert" {
		t.Errorf("the default's %+v", def.Spec.TLS)
	}

	// Passthrough and Terminate on one port conflict
	passthrough := l4Listener("passthrough", 8443, gatewayv1.TLSProtocolType, "p.example.com")
	b.gateway("gw", terminate, passthrough)
	res := build(t, o.Controller, b)
	if len(res.TransportServers) != 0 {
		t.Errorf("mixed modes served %d TransportServers", len(res.TransportServers))
	}
	for _, l := range res.Graph.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}].Listeners {
		conflict := false
		for _, c := range l.Conditions {
			if c.Type == string(gatewayv1.ListenerConditionAccepted) && c.Reason == string(gatewayv1.ListenerReasonProtocolConflict) {
				conflict = true
			}
		}
		if l.Valid || !conflict || len(l.SupportedKinds) != 0 {
			t.Errorf("listener %s: valid %v, conflict %v, kinds %v", l.Name, l.Valid, conflict, l.SupportedKinds)
		}
	}
}

func TestTCPRouteBackendsInTwoNamespaces(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	other := "other"
	b := newState().gateway("gw", l4Listener("tcp", 5000, gatewayv1.TCPProtocolType, "")).service("a", 5000)
	b.s.Namespaces[types.NamespacedName{Name: other}] = &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: other}}
	b.s.Services[types.NamespacedName{Namespace: other, Name: "b"}] = &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "b"},
		Spec:       v1.ServiceSpec{Ports: []v1.ServicePort{{Port: 5000, TargetPort: intstr.FromInt32(8080)}}},
	}
	far := backendRef("b", 5000, 1)
	far.Namespace = ptr(gatewayv1.Namespace(other))
	b.tcpRoute("t", parentRef("gw", "tcp"), backendRef("a", 5000, 1), far)

	// without a ReferenceGrant the other namespace's backend is not served
	tcp := validTransportServers(t, build(t, o.Controller, b))["TCP 5000 "]
	if tcp == nil {
		t.Fatal("no TransportServer")
	}
	for _, u := range tcp.Spec.Upstreams {
		if strings.Contains(u.Service, "/") {
			t.Errorf("served a backend no ReferenceGrant allows: %+v", tcp.Spec.Upstreams)
		}
	}

	b.s.ReferenceGrants[types.NamespacedName{Namespace: other, Name: "allow"}] = &gatewayv1.ReferenceGrant{
		ObjectMeta: metav1.ObjectMeta{Namespace: other, Name: "allow"},
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "TCPRoute", Namespace: appNs}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Service"}},
		},
	}
	tcp = validTransportServers(t, build(t, newOwnAddressController(t).Controller, b))["TCP 5000 "]
	if tcp == nil || tcp.Namespace != appNs || len(tcp.Spec.Upstreams) != 2 {
		t.Fatalf("TransportServer %+v", tcp)
	}
	services := []string{tcp.Spec.Upstreams[0].Service, tcp.Spec.Upstreams[1].Service}
	slices.Sort(services)
	if !slices.Equal(services, []string{"a", "other/b"}) {
		t.Errorf("services %q", services)
	}
}
