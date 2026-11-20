package k8s

import (
	"testing"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"

	"github.com/wx-one/wxingress/internal/gateway"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

func gatewayVS(name, host string, generation int64) *conf_v1.VirtualServer {
	return &conf_v1.VirtualServer{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   "infra",
			Name:        gateway.NamePrefix + name,
			Generation:  generation,
			Annotations: map[string]string{gateway.AnnotationGateway: "infra/gw"},
		},
		Spec: conf_v1.VirtualServerSpec{
			IngressClass: "nginx",
			Host:         host,
			Upstreams:    []conf_v1.Upstream{{Name: "be-1", Service: "app/a", Port: 80}},
			Routes:       []conf_v1.Route{{Path: "/", Action: &conf_v1.Action{Pass: "be-1"}}},
		},
	}
}

func changeKeys(changes []ResourceChange) map[string]Operation {
	out := make(map[string]Operation)
	for _, c := range changes {
		out[c.Resource.GetKeyWithKind()] = c.Op
	}
	return out
}

func TestReplaceGatewayVirtualServers(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()

	a := gatewayVS("a", "a.example.com", 1)
	all := gatewayVS("all", "~^", 1)
	changes, problems := c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{a, all}, nil)
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	got := changeKeys(changes)
	if got["VirtualServer/infra/GW-a"] != AddOrUpdate || got["VirtualServer/infra/GW-all"] != AddOrUpdate || len(got) != 2 {
		t.Errorf("first replace: changes %v", got)
	}

	// the same generations change nothing
	changes, problems = c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{gatewayVS("a", "a.example.com", 1), all}, nil)
	if len(changes) != 0 || len(problems) != 0 {
		t.Errorf("unchanged replace: changes %v problems %v", changeKeys(changes), problems)
	}

	// a new generation updates, a missing one goes
	a2 := gatewayVS("a", "a.example.com", 2)
	a2.Spec.Routes[0].Path = "/v2/"
	changes, _ = c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{a2}, nil)
	got = changeKeys(changes)
	if got["VirtualServer/infra/GW-a"] != AddOrUpdate || got["VirtualServer/infra/GW-all"] != Delete {
		t.Errorf("second replace: changes %v", got)
	}

	// an invalid one is a problem and is removed
	bad := gatewayVS("a", "a.example.com", 3)
	bad.Spec.Routes[0].Action.Pass = "no-such-upstream"
	changes, problems = c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{bad}, nil)
	if len(problems) != 1 || changeKeys(changes)["VirtualServer/infra/GW-a"] != Delete {
		t.Errorf("invalid replace: changes %v problems %v", changeKeys(changes), problems)
	}
}

func TestReplaceGatewayVirtualServersKeepsOtherVirtualServers(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	own := createTestVirtualServer("own", "own.example.com")
	c.AddOrUpdateVirtualServer(own)

	c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{gatewayVS("a", "a.example.com", 1)}, nil)
	c.ReplaceGatewayVirtualServers(nil, nil)

	if c.GetVirtualServer("default/own") == nil {
		t.Error("replacing the Gateway VirtualServers removed one from the API")
	}
	if c.GetVirtualServer("infra/GW-a") != nil {
		t.Error("a Gateway VirtualServer stayed")
	}
}

func TestGatewaysWithOwnListenersDoNotShareHosts(t *testing.T) {
	t.Parallel()
	c := createTestConfiguration()
	// an Ingress-like VirtualServer from the API has the host on the default ports
	c.AddOrUpdateVirtualServer(createTestVirtualServer("own", "x.example.com"))

	onListener := func(name, host, listener string) *conf_v1.VirtualServer {
		vs := gatewayVS(name, host, 1)
		vs.Spec.Listener = &conf_v1.VirtualServerListener{HTTP: listener}
		return vs
	}
	a := onListener("a", "~^", gateway.NamePrefix+"a-80")
	b := onListener("b", "~^", gateway.NamePrefix+"b-80")
	x := onListener("x", "x.example.com", gateway.NamePrefix+"b-80")
	listeners := []conf_v1.Listener{
		{Name: gateway.NamePrefix + "a-80", Protocol: conf_v1.HTTPProtocol, Port: 20001},
		{Name: gateway.NamePrefix + "b-80", Protocol: conf_v1.HTTPProtocol, Port: 20002},
	}
	changes, problems := c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{a, b, x}, listeners)
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	got := changeKeys(changes)
	if len(got) != 3 {
		t.Fatalf("changes %v, want the three Gateway VirtualServers", got)
	}
	ports := map[string]int{}
	for _, ch := range changes {
		vsc := ch.Resource.(*VirtualServerConfiguration)
		ports[vsc.VirtualServer.Name] = vsc.HTTPPort
	}
	want := map[string]int{gateway.NamePrefix + "a": 20001, gateway.NamePrefix + "b": 20002, gateway.NamePrefix + "x": 20002}
	if !cmp.Equal(ports, want) {
		t.Errorf("ports %v, want %v", ports, want)
	}

	// a listener that moves to another port updates its VirtualServers
	listeners[1].Port = 20003
	changes, _ = c.ReplaceGatewayVirtualServers([]*conf_v1.VirtualServer{a, b, x}, listeners)
	got = changeKeys(changes)
	if got["VirtualServer/infra/GW-b"] != AddOrUpdate || got["VirtualServer/infra/GW-x"] != AddOrUpdate || len(got) != 2 {
		t.Errorf("changes after the port moved: %v", got)
	}
}

func TestGatewayDefaultServerPorts(t *testing.T) {
	t.Parallel()
	http, https := gatewayDefaultServerPorts([]conf_v1.Listener{
		{Name: "GW-a-80", Protocol: conf_v1.HTTPProtocol, Port: 20002},
		{Name: "GW-b-80", Protocol: conf_v1.HTTPProtocol, Port: 20001},
		{Name: "GW-b-443", Protocol: conf_v1.HTTPProtocol, Port: 20003, Ssl: true},
		{Name: "GW-c-53", Protocol: "UDP", Port: 20004},
	})
	if !cmp.Equal(http, []int{20001, 20002}) || !cmp.Equal(https, []int{20003}) {
		t.Errorf("http %v https %v", http, https)
	}
}

func TestGatewayEventFilter(t *testing.T) {
	t.Parallel()
	fake := record.NewFakeRecorder(10)
	f := gatewayEventFilter{fake}
	f.Eventf(gatewayVS("a", "a.example.com", 1), "Normal", "AddedOrUpdated", "x")
	f.Eventf(createTestVirtualServer("own", "own.example.com"), "Normal", "AddedOrUpdated", "y")
	if len(fake.Events) != 1 {
		t.Fatalf("got %d events, want only the one of the VirtualServer from the API", len(fake.Events))
	}
	if e := <-fake.Events; e != "Normal AddedOrUpdated y" {
		t.Errorf("event %q", e)
	}
}
