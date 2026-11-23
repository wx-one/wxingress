package gateway

import (
	"context"
	"log/slog"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

// labelAllowed is the label of the namespaces the default Gateway of the
// tests takes ListenerSets from.
const labelAllowed = "wxingress.wizardtales.com/default-gateway"

// allowListenerSets lets the Gateway gw take ListenerSets from the
// namespaces with labelAllowed=allowed, and gives appNs that label.
func (b *stateBuilder) allowListenerSets(gw string) *stateBuilder {
	b.s.Gateways[types.NamespacedName{Namespace: gwNs, Name: gw}].Spec.AllowedListeners = &gatewayv1.AllowedListeners{
		Namespaces: &gatewayv1.ListenerNamespaces{
			From:     ptr(gatewayv1.NamespacesFromSelector),
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{labelAllowed: "allowed"}},
		},
	}
	b.s.Namespaces[types.NamespacedName{Name: appNs}].Labels = map[string]string{labelAllowed: "allowed"}
	return b
}

// listenerSet adds a ListenerSet of appNs, attached to the Gateway gw of
// gwNs, created at created.
func (b *stateBuilder) listenerSet(name, gw string, created time.Time, listeners ...gatewayv1.Listener) *stateBuilder {
	if b.s.ListenerSets == nil {
		b.s.ListenerSets = map[types.NamespacedName]*gatewayv1.ListenerSet{}
	}
	entries := make([]gatewayv1.ListenerEntry, 0, len(listeners))
	for _, l := range listeners {
		entries = append(entries, gatewayv1.ListenerEntry(l))
	}
	b.s.ListenerSets[types.NamespacedName{Namespace: appNs, Name: name}] = &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: name, Generation: 1, CreationTimestamp: metav1.NewTime(created)},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: gatewayv1.ObjectName(gw), Namespace: ptr(gatewayv1.Namespace(gwNs))},
			Listeners: entries,
		},
	}
	return b
}

// toListenerSet attaches the route name to the listener section of the
// ListenerSet ls instead of the Gateway.
func (b *stateBuilder) toListenerSet(route, ls, section string) *stateBuilder {
	b.s.HTTPRoutes[routeKey(route)].Spec.ParentRefs = []gatewayv1.ParentReference{{
		Group:       ptr(gatewayv1.Group(gatewayv1.GroupName)),
		Kind:        ptr(gatewayv1.Kind("ListenerSet")),
		Name:        gatewayv1.ObjectName(ls),
		SectionName: ptr(gatewayv1.SectionName(section)),
	}}
	return b
}

func listenerSetOf(t *testing.T, res Result, name string) *graph.ListenerSet {
	t.Helper()
	ls := res.Graph.ListenerSets[types.NamespacedName{Namespace: appNs, Name: name}]
	if ls == nil {
		t.Fatalf("ListenerSet %s is not in the graph", name)
	}
	return ls
}

func conditionOf(conds []metav1.Condition, typ string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == typ {
			return &conds[i]
		}
	}
	return nil
}

func hasListenerCondition(l *graph.Listener, typ string, status metav1.ConditionStatus, reason string) bool {
	for _, c := range l.Conditions {
		if c.Type == typ && c.Status == status && (reason == "" || c.Reason == reason) {
			return true
		}
	}
	return false
}

// TestListenerSetServesItsCertificateOnTheDefaultGateway: a namespace with
// no access to the default Gateway's adds an HTTPS listener for its
// hostname with its own certificate, and a route on that listener is
// served from the shared address with that certificate.
func TestListenerSetServesItsCertificateOnTheDefaultGateway(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", listenerOn("http", 80, "")).
		allowListenerSets("gw").
		listenerSet("app", "gw", time.Now(), httpsListener("https", 443, "app.example.com", "", "app-tls")).
		secret(tlsSecret(t, appNs, "app-tls", "app.example.com")).
		service("a", 80).
		route("r", []string{"app.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}}).
		toListenerSet("r", "app", "https")

	res := build(t, testController(), b)
	vs := onlyVS(t, res)
	if vs.Spec.Host != "app.example.com" || vs.Spec.TLS == nil || vs.Spec.TLS.Secret != "app-tls" || vs.Namespace != appNs {
		t.Fatalf("host %s namespace %s tls %+v", vs.Spec.Host, vs.Namespace, vs.Spec.TLS)
	}
	// the VirtualServer is in the namespace of the certificate, the route's
	if len(vs.Spec.Upstreams) != 1 || vs.Spec.Upstreams[0].Service != "a" {
		t.Errorf("upstreams %+v", vs.Spec.Upstreams)
	}
	ls := listenerSetOf(t, res, "app")
	if !ls.Valid || len(ls.Listeners) != 1 || !ls.Listeners[0].Valid || len(ls.Listeners[0].Routes) != 1 {
		t.Errorf("ListenerSet valid=%v listeners %+v", ls.Valid, ls.Listeners)
	}
	gw := res.Graph.Gateways[types.NamespacedName{Namespace: gwNs, Name: "gw"}]
	if _, ok := gw.AttachedListenerSets[types.NamespacedName{Namespace: appNs, Name: "app"}]; !ok {
		t.Error("the ListenerSet is not attached to the Gateway")
	}
}

// TestListenerSetOfANamespaceNotAllowedIsNotServed: a Gateway takes
// ListenerSets only from the namespaces spec.allowedListeners names, and
// none without it.
func TestListenerSetOfANamespaceNotAllowedIsNotServed(t *testing.T) {
	t.Parallel()
	for name, allow := range map[string]bool{"no allowedListeners": false, "namespace without the label": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newState().
				gateway("gw", listenerOn("http", 80, "")).
				listenerSet("app", "gw", time.Now(), httpsListener("https", 443, "app.example.com", "", "app-tls")).
				secret(tlsSecret(t, appNs, "app-tls", "app.example.com")).
				service("a", 80).
				route("r", []string{"app.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}}).
				toListenerSet("r", "app", "https")
			if allow {
				b.allowListenerSets("gw")
				b.s.Namespaces[types.NamespacedName{Name: appNs}].Labels = nil
			}
			res := build(t, testController(), b)
			if len(res.VirtualServers) != 0 {
				t.Fatalf("served %s from a ListenerSet the Gateway does not allow", res.VirtualServers[0].Spec.Host)
			}
			ls := listenerSetOf(t, res, "app")
			if ls.Valid {
				t.Error("the ListenerSet is valid")
			}
			var accepted bool
			for _, c := range ls.Conditions {
				if c.Type == string(gatewayv1.ListenerSetConditionAccepted) && c.Status == metav1.ConditionFalse &&
					c.Reason == string(gatewayv1.ListenerSetReasonNotAllowed) {
					accepted = true
				}
			}
			if !accepted {
				t.Errorf("conditions %+v, want Accepted False NotAllowed", ls.Conditions)
			}
		})
	}
}

// TestListenerSetOnTheDefaultGatewayOnlyOnItsPorts: the default Gateway
// shares the controller's address, which serves HTTP on 80 and HTTPS on 443
// only - a ListenerSet cannot open another port on it.
func TestListenerSetOnTheDefaultGatewayOnlyOnItsPorts(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", listenerOn("http", 80, "")).
		allowListenerSets("gw").
		listenerSet("app", "gw", time.Now(), httpsListener("alt", 8443, "app.example.com", "", "app-tls")).
		secret(tlsSecret(t, appNs, "app-tls", "app.example.com")).
		service("a", 80).
		route("r", []string{"app.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}}).
		toListenerSet("r", "app", "alt")

	res := build(t, testController(), b)
	if len(res.VirtualServers) != 0 {
		t.Fatalf("served %s on port 8443 of the default Gateway", res.VirtualServers[0].Spec.Host)
	}
	l := listenerSetOf(t, res, "app").Listeners[0]
	if l.Valid || !hasListenerCondition(l, string(gatewayv1.ListenerConditionAccepted), metav1.ConditionFalse, string(gatewayv1.ListenerReasonPortUnavailable)) {
		t.Errorf("listener valid=%v conditions %+v, want PortUnavailable", l.Valid, l.Conditions)
	}
}

// TestListenerSetCannotTakeAHostnameOfTheGateway: a listener of a
// ListenerSet on a port and hostname a listener of the Gateway has is a
// conflict, and the Gateway's listener and certificate keep it; between
// two ListenerSets the older one keeps it.
func TestListenerSetCannotTakeAHostnameOfTheGateway(t *testing.T) {
	t.Parallel()
	now := time.Now()
	b := newState().
		gateway("gw", listenerOn("http", 80, ""), httpsListener("https", 443, "app.example.com", "", "gw-tls")).
		allowListenerSets("gw").
		secret(tlsSecret(t, gwNs, "gw-tls", "app.example.com")).
		listenerSet("new", "gw", now, httpsListener("https", 443, "app.example.com", "", "app-tls"),
			httpsListener("other", 443, "other.example.com", "", "app-tls")).
		listenerSet("old", "gw", now.Add(-time.Hour), httpsListener("https", 443, "other.example.com", "", "old-tls")).
		secret(tlsSecret(t, appNs, "app-tls", "app.example.com", "other.example.com")).
		secret(tlsSecret(t, appNs, "old-tls", "other.example.com")).
		service("a", 80).
		route("r", []string{"app.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})

	res := build(t, testController(), b)
	certs := map[string]string{}
	for _, vs := range res.VirtualServers {
		if vs.Spec.TLS != nil {
			certs[vs.Spec.Host] = vs.Namespace + "/" + vs.Spec.TLS.Secret
		}
	}
	if certs["app.example.com"] != gwNs+"/gw-tls" {
		t.Errorf("app.example.com has %q, want the Gateway's certificate", certs["app.example.com"])
	}
	if c, ok := certs["other.example.com"]; ok && c != appNs+"/old-tls" {
		t.Errorf("other.example.com has %q, want the older ListenerSet's certificate", c)
	}
	for _, l := range listenerSetOf(t, res, "new").Listeners {
		if l.Valid || !hasListenerCondition(l, string(gatewayv1.ListenerConditionConflicted), metav1.ConditionTrue, "") {
			t.Errorf("listener %s of the newer ListenerSet valid=%v conditions %+v, want Conflicted", l.Name, l.Valid, l.Conditions)
		}
	}
	if old := listenerSetOf(t, res, "old"); !old.Valid || !old.Listeners[0].Valid {
		t.Errorf("the older ListenerSet valid=%v listeners %+v", old.Valid, old.Listeners)
	}
}

// TestListenerSetCannotTakeTheRoutesOfTheGatewayOff: a listener of a
// ListenerSet may have the name of a listener of the Gateway. The routes
// attached to the Gateway without a sectionName stay attached to its own
// listeners - NGF v2.8.1 failed them as "duplicate section name", without a
// status (hack/ngf-patches/0002).
func TestListenerSetCannotTakeTheRoutesOfTheGatewayOff(t *testing.T) {
	t.Parallel()
	b := newState().
		gateway("gw", listenerOn("http", 80, "")).
		allowListenerSets("gw").
		listenerSet("app", "gw", time.Now(), httpsListener("http", 443, "app.example.com", "", "app-tls")).
		secret(tlsSecret(t, appNs, "app-tls", "app.example.com")).
		service("a", 80).
		route("other", []string{"other.example.com"}, gatewayv1.HTTPRouteRule{BackendRefs: []gatewayv1.HTTPBackendRef{backend("a", 80, 1)}})

	res := build(t, testController(), b)
	vs := onlyVS(t, res)
	if vs.Spec.Host != "other.example.com" {
		t.Errorf("host %s", vs.Spec.Host)
	}
	r := res.Graph.Routes[graph.RouteKey{NamespacedName: routeKey("other"), RouteType: graph.RouteTypeHTTP}]
	if r == nil || !r.Valid || len(r.ParentRefs) != 1 || r.ParentRefs[0].Attachment == nil || !r.ParentRefs[0].Attachment.Attached {
		t.Fatalf("the route of the Gateway is not attached: %+v", r)
	}
	if ls := listenerSetOf(t, res, "app"); !ls.Valid || !ls.Listeners[0].Valid {
		t.Errorf("the ListenerSet valid=%v listeners %+v", ls.Valid, ls.Listeners)
	}
}

// TestListenerSetsAreWatchedAndGetAStatus: with the ListenerSet CRD the
// controller lists the ListenerSets of the watched namespaces, and the
// leader writes their status and the Gateway's attachedListenerSets.
func TestListenerSetsAreWatchedAndGetAStatus(t *testing.T) {
	t.Parallel()
	gw := gatewayIn(gwNs, "gw")
	gw.Spec.Listeners = []gatewayv1.Listener{listenerOn("http", 80, "")}
	gw.Spec.AllowedListeners = &gatewayv1.AllowedListeners{Namespaces: &gatewayv1.ListenerNamespaces{From: ptr(gatewayv1.NamespacesFromAll)}}
	gwc := gatewayClient(t, gw)
	ctx := context.Background()
	if _, err := gwc.GatewayV1().GatewayClasses().Create(ctx, &gatewayv1.GatewayClass{
		ObjectMeta: metav1.ObjectMeta{Name: DefaultGatewayClass},
		Spec:       gatewayv1.GatewayClassSpec{ControllerName: DefaultControllerName},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	ls := &gatewayv1.ListenerSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: appNs, Name: "app"},
		Spec: gatewayv1.ListenerSetSpec{
			ParentRef: gatewayv1.ParentGatewayReference{Name: "gw", Namespace: ptr(gatewayv1.Namespace(gwNs))},
			Listeners: []gatewayv1.ListenerEntry{gatewayv1.ListenerEntry(httpsListener("https", 443, "app.example.com", "", "app-tls"))},
		},
	}
	if _, err := gwc.GatewayV1().ListenerSets(appNs).Create(ctx, ls, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	secret := tlsSecret(t, appNs, "app-tls", "app.example.com")
	kc := kubefake.NewClientset(
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: gwNs}},
		&v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: appNs}},
	)
	c := New(Config{
		KubeClient:      kc,
		GatewayClient:   gwc,
		Logger:          slog.New(slog.DiscardHandler),
		Namespace:       "wxingress",
		DefaultGateway:  types.NamespacedName{Namespace: gwNs, Name: "gw"},
		WatchNamespaces: []string{""},
		ListenerSets:    true,
	})
	c.SetSources(fakeSources{secrets: []*v1.Secret{secret}})
	c.SetIsLeader(func() bool { return true })
	stop := make(chan struct{})
	defer close(stop)
	c.Start(stop)
	if !cache.WaitForCacheSync(stop, c.HasSynced()...) {
		t.Fatal("no sync")
	}

	res, err := c.Build(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.WriteStatus(ctx, res, nil, nil)

	got, err := gwc.GatewayV1().ListenerSets(appNs).Get(ctx, "app", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a := conditionOf(got.Status.Conditions, string(gatewayv1.ListenerSetConditionAccepted)); a == nil || a.Status != metav1.ConditionTrue {
		t.Errorf("ListenerSet conditions %+v, want Accepted", got.Status.Conditions)
	}
	if len(got.Status.Listeners) != 1 || got.Status.Listeners[0].Name != "https" {
		t.Errorf("listener status %+v", got.Status.Listeners)
	}
	gotGW, err := gwc.GatewayV1().Gateways(gwNs).Get(ctx, "gw", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if gotGW.Status.AttachedListenerSets == nil || *gotGW.Status.AttachedListenerSets != 1 {
		t.Errorf("attachedListenerSets %v, want 1", gotGW.Status.AttachedListenerSets)
	}
	for _, l := range gotGW.Status.Listeners {
		if l.Name == "https" {
			t.Error("the Gateway reports the ListenerSet's listener as its own")
		}
	}
}

// TestNoListenerSetsWithoutTheCRD: without the CRD no ListenerSets are
// watched, and the informers still sync.
func TestNoListenerSetsWithoutTheCRD(t *testing.T) {
	t.Parallel()
	c := New(Config{
		KubeClient:      kubefake.NewClientset(),
		GatewayClient:   gatewayClient(t, gatewayIn("a", "gw")),
		Logger:          slog.New(slog.DiscardHandler),
		WatchNamespaces: []string{""},
	})
	stop := make(chan struct{})
	defer close(stop)
	c.Start(stop)
	if !cache.WaitForCacheSync(stop, c.HasSynced()...) {
		t.Fatal("no sync")
	}
	state, err := c.clusterState()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.ListenerSets) != 0 {
		t.Errorf("ListenerSets %v without the CRD", state.ListenerSets)
	}
	if _, err := c.watchOf("a").listenerSet("a", "x"); err == nil {
		t.Error("a ListenerSet was found without the CRD")
	}
}
