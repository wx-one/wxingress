package gateway

import (
	"context"
	"log/slog"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwfake "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned/fake"
)

type fakeSources struct {
	services []*v1.Service
	secrets  []*v1.Secret
}

func (f fakeSources) ListServices() []*v1.Service { return f.services }
func (f fakeSources) ListSecrets() []*v1.Secret   { return f.secrets }

func gatewayIn(ns, name string) *gatewayv1.Gateway {
	return &gatewayv1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       gatewayv1.GatewaySpec{GatewayClassName: DefaultGatewayClass},
	}
}

// gatewayClient is a fake client with gws. They are created through it: the
// fake's tracker would file objects given to it under v1beta1, whose
// Gateway is an alias of v1's.
func gatewayClient(t *testing.T, gws ...*gatewayv1.Gateway) *gwfake.Clientset {
	t.Helper()
	c := gwfake.NewSimpleClientset() //nolint:staticcheck // NewClientset knows no Gateways of v1
	for _, gw := range gws {
		if _, err := c.GatewayV1().Gateways(gw.Namespace).Create(context.Background(), gw, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func gatewaysOf(t *testing.T, c *Controller) []string {
	t.Helper()
	state, err := c.clusterState()
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for k := range state.Gateways {
		out = append(out, k.String())
	}
	slices.Sort(out)
	return out
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within 5s: %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestWatchedNamespacesOnly(t *testing.T) {
	t.Parallel()
	gwc := gatewayClient(t, gatewayIn("a", "gw"), gatewayIn("b", "gw"))
	kc := kubefake.NewClientset()
	c := New(Config{
		KubeClient:      kc,
		GatewayClient:   gwc,
		Logger:          slog.New(slog.DiscardHandler),
		Namespace:       "wxingress",
		WatchNamespaces: []string{"a"},
	})
	svc := &v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "svc"}}
	c.SetSources(fakeSources{services: []*v1.Service{svc}})
	var changes atomic.Int32
	if err := c.OnChange(func() { changes.Add(1) }); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	c.Start(stop)
	if !cache.WaitForCacheSync(stop, c.HasSynced()...) {
		t.Fatal("no sync")
	}

	if got := gatewaysOf(t, c); !slices.Equal(got, []string{"a/gw"}) {
		t.Errorf("gateways %v, want a's alone", got)
	}
	state, _ := c.clusterState()
	if state.Services[types.NamespacedName{Namespace: "a", Name: "svc"}] != svc {
		t.Error("the Services do not come from the sources")
	}

	// a namespace that gets the label
	before := changes.Load()
	c.WatchNamespace("b")
	eventually(t, "b is watched", func() bool { return slices.Equal(gatewaysOf(t, c), []string{"a/gw", "b/gw"}) })
	eventually(t, "a change is asked for", func() bool { return changes.Load() > before })

	// and one that loses it
	c.UnwatchNamespace("a")
	if got := gatewaysOf(t, c); !slices.Equal(got, []string{"b/gw"}) {
		t.Errorf("gateways %v after a was unwatched", got)
	}
	if _, err := c.cachedGateway("a", "gw"); err == nil {
		t.Error("a Gateway of an unwatched namespace is still in the cache")
	}
}

func TestAllNamespaces(t *testing.T) {
	t.Parallel()
	c := New(Config{
		KubeClient:      kubefake.NewClientset(),
		GatewayClient:   gatewayClient(t, gatewayIn("a", "gw"), gatewayIn("b", "gw")),
		Logger:          slog.New(slog.DiscardHandler),
		WatchNamespaces: []string{""},
	})
	stop := make(chan struct{})
	defer close(stop)
	c.Start(stop)
	if !cache.WaitForCacheSync(stop, c.HasSynced()...) {
		t.Fatal("no sync")
	}
	if got := gatewaysOf(t, c); !slices.Equal(got, []string{"a/gw", "b/gw"}) {
		t.Errorf("gateways %v", got)
	}
	if _, err := c.cachedGateway("b", "gw"); err != nil {
		t.Errorf("cached: %v", err)
	}
}

// TestOwnServicesOfAllNamespaces: the Services of the class's Gateways are
// listed in every namespace, watched or not, and nothing else is.
func TestOwnServicesOfAllNamespaces(t *testing.T) {
	t.Parallel()
	mine := map[string]string{LabelManagedBy: managedBy, LabelGatewayClass: DefaultGatewayClass}
	other := map[string]string{LabelManagedBy: managedBy, LabelGatewayClass: "another"}
	kc := kubefake.NewClientset(
		&v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "watched", Labels: mine}},
		&v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "left", Labels: mine}},
		&v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "another-class", Labels: other}},
		&v1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: "b", Name: "app"}},
	)
	c := New(Config{
		KubeClient:      kc,
		GatewayClient:   gatewayClient(t),
		Logger:          slog.New(slog.DiscardHandler),
		Namespace:       "wxingress",
		WatchNamespaces: []string{"a"},
	})
	stop := make(chan struct{})
	defer close(stop)
	c.Start(stop)
	if !cache.WaitForCacheSync(stop, c.HasSynced()...) {
		t.Fatal("no sync")
	}
	svcs, err := c.ownServices()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range svcs {
		got = append(got, s.Namespace+"/"+s.Name)
	}
	slices.Sort(got)
	if !slices.Equal(got, []string{"a/watched", "b/left"}) {
		t.Errorf("own Services %v, want a/watched and b/left", got)
	}
	if !c.syncedFor("b") {
		t.Error("an unwatched namespace counts as not synced")
	}
}
