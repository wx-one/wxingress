package gateway

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

func httpsListener(name string, port int32, hostname string, certNs, cert string) gatewayv1.Listener {
	l := listenerOn(name, port, hostname)
	l.Protocol = gatewayv1.HTTPSProtocolType
	ref := gatewayv1.SecretObjectReference{Name: gatewayv1.ObjectName(cert)}
	if certNs != "" {
		ref.Namespace = ptr(gatewayv1.Namespace(certNs))
	}
	l.TLS = &gatewayv1.ListenerTLSConfig{Mode: ptr(gatewayv1.TLSModeTerminate), CertificateRefs: []gatewayv1.SecretObjectReference{ref}}
	return l
}

func TestHTTPAndHTTPSOfTheDefaultGatewayApart(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", listenerOn("http", 80, "x.example.com"), httpsListener("https", 443, "x.example.com", "", "cert")).
		secret(tlsSecret(t, gwNs, "cert", "x.example.com")).
		service("a", 80).service("b", 80)
	b.route("plain", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	b.s.HTTPRoutes[routeKey("plain")].Spec.ParentRefs[0].SectionName = ptr(gatewayv1.SectionName("http"))
	b.route("secure", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("b", 80, 1)}})
	b.s.HTTPRoutes[routeKey("secure")].Spec.ParentRefs[0].SectionName = ptr(gatewayv1.SectionName("https"))

	vs := onlyVS(t, build(t, testController(), b))
	if vs.Spec.TLS == nil || vs.Spec.TLS.Secret != "cert" {
		t.Fatalf("tls %+v", vs.Spec.TLS)
	}
	r := plainRoutes(vs.Spec.Routes)
	if len(r) != 1 || len(r[0].Matches) != 2 {
		t.Fatalf("routes %+v", r)
	}
	ups := map[string]string{}
	for _, u := range vs.Spec.Upstreams {
		ups[u.Name] = u.Service
	}
	got := map[string]string{}
	for _, m := range r[0].Matches {
		if len(m.Conditions) != 1 || m.Conditions[0].Variable != "$scheme" {
			t.Fatalf("conditions %+v", m.Conditions)
		}
		got[m.Conditions[0].Value] = ups[m.Action.Pass]
	}
	if !cmp.Equal(got, map[string]string{"http": "app/a", "https": "app/b"}) {
		t.Errorf("by scheme %v", got)
	}
	if r[0].Action == nil || r[0].Action.Return == nil || r[0].Action.Return.Code != 404 {
		t.Errorf("default action %+v", r[0].Action)
	}
}

func TestHTTPSListenerWithoutRoutesHasItsCertificate(t *testing.T) {
	t.Parallel()
	o := newOwnAddressController(t)
	b := newState().gateway("gw", httpsListener("https", 443, "", "", "cert")).secret(tlsSecret(t, gwNs, "cert", "x.example.com"))
	vs := onlyVS(t, build(t, o.Controller, b))
	if vs.Spec.TLS == nil || vs.Spec.Listener == nil || vs.Spec.Listener.HTTPS == "" || !IsCatchAllHost(vs.Spec.Host) {
		t.Fatalf("vs %+v", vs.Spec)
	}
	if len(vs.Spec.Routes) != 1 || vs.Spec.Routes[0].Action.Return == nil || vs.Spec.Routes[0].Action.Return.Code != 404 {
		t.Errorf("routes %+v", vs.Spec.Routes)
	}
}

func TestCertificateFromAnotherNamespace(t *testing.T) {
	t.Parallel()
	const certNs = "certs"
	b := newState().
		gateway("gw", httpsListener("https", 443, "x.example.com", certNs, "cert")).
		secret(tlsSecret(t, certNs, "cert", "x.example.com")).
		service("a", 80).
		route("r", []string{"x.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})
	b.s.Namespaces[types.NamespacedName{Name: certNs}] = b.s.Namespaces[types.NamespacedName{Name: gwNs}].DeepCopy()
	b.s.Namespaces[types.NamespacedName{Name: certNs}].Name = certNs

	// without a ReferenceGrant the listener is not valid
	if res := build(t, testController(), b); len(res.VirtualServers) != 0 {
		t.Fatalf("served a certificate no ReferenceGrant allows: %+v", res.VirtualServers[0].Spec.TLS)
	}

	b.s.ReferenceGrants[types.NamespacedName{Namespace: certNs, Name: "allow"}] = &gatewayv1.ReferenceGrant{
		ObjectMeta: metaOf(certNs, "allow"),
		Spec: gatewayv1.ReferenceGrantSpec{
			From: []gatewayv1.ReferenceGrantFrom{{Group: gatewayv1.GroupName, Kind: "Gateway", Namespace: gwNs}},
			To:   []gatewayv1.ReferenceGrantTo{{Group: "", Kind: "Secret"}},
		},
	}
	vs := onlyVS(t, build(t, testController(), b))
	if vs.Namespace != certNs || vs.Spec.TLS == nil || vs.Spec.TLS.Secret != "cert" {
		t.Errorf("namespace %s tls %+v", vs.Namespace, vs.Spec.TLS)
	}
	if vs.Annotations[AnnotationGateway] != gwNs+"/gw" {
		t.Errorf("gateway %s", vs.Annotations[AnnotationGateway])
	}
	if len(vs.Spec.Upstreams) != 1 || vs.Spec.Upstreams[0].Service != appNs+"/a" {
		t.Errorf("upstreams %+v", vs.Spec.Upstreams)
	}
	_ = conf_v1.TLS{}
}

func metaOf(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
