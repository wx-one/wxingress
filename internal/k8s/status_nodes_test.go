package k8s

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	nic_glog "github.com/wx-one/wxingress/internal/logger/glog"
	"github.com/wx-one/wxingress/internal/logger/levels"
	api_v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func quietLogger() *slog.Logger {
	return slog.New(nic_glog.New(io.Discard, &nic_glog.Options{Level: levels.LevelInfo}))
}

func statusAddresses(su *statusUpdater) []string {
	var out []string
	for _, s := range su.status {
		out = append(out, s.IP+s.Hostname)
	}
	return out
}

// A DaemonSet on the nodes' ports behind a ClusterIP Service: the status is
// the nodes' addresses, as with ingress-nginx without --publish-service.
func TestStatusIsTheControllersNodesWhereNothingElseGivesAnAddress(t *testing.T) {
	t.Parallel()
	su := &statusUpdater{logger: quietLogger()}
	su.SaveStatusFromExternalService(&api_v1.Service{Spec: api_v1.ServiceSpec{Type: api_v1.ServiceTypeClusterIP}})

	if !su.SaveStatusFromNodes([]string{"10.0.0.1", "10.0.0.2"}) {
		t.Fatal("the nodes' addresses did not change the status")
	}
	if got, want := statusAddresses(su), []string{"10.0.0.1", "10.0.0.2"}; !slices.Equal(got, want) {
		t.Errorf("status %v, want %v", got, want)
	}
	if su.SaveStatusFromNodes([]string{"10.0.0.1", "10.0.0.2"}) {
		t.Error("the same addresses changed the status again")
	}
	if !su.SaveStatusFromNodes([]string{"10.0.0.1"}) {
		t.Error("a node gone did not change the status")
	}
	if got, want := statusAddresses(su), []string{"10.0.0.1"}; !slices.Equal(got, want) {
		t.Errorf("status %v, want %v", got, want)
	}
}

// The Service's address and external-status-address win over the nodes, and
// the nodes come back when those go.
func TestStatusOfTheServiceAndTheConfigMapWinOverTheNodes(t *testing.T) {
	t.Parallel()
	su := &statusUpdater{logger: quietLogger()}
	su.SaveStatusFromNodes([]string{"10.0.0.1"})

	lb := &api_v1.Service{
		Spec:   api_v1.ServiceSpec{Type: api_v1.ServiceTypeLoadBalancer},
		Status: api_v1.ServiceStatus{LoadBalancer: api_v1.LoadBalancerStatus{Ingress: []api_v1.LoadBalancerIngress{{IP: "203.0.113.1"}}}},
	}
	su.SaveStatusFromExternalService(lb)
	if got, want := statusAddresses(su), []string{"203.0.113.1"}; !slices.Equal(got, want) {
		t.Errorf("with a LoadBalancer Service: status %v, want %v", got, want)
	}
	if su.SaveStatusFromNodes([]string{"10.0.0.1", "10.0.0.2"}) {
		t.Error("the nodes changed the status of a LoadBalancer Service")
	}

	su.SaveStatusFromExternalStatus("198.51.100.1")
	if got, want := statusAddresses(su), []string{"198.51.100.1"}; !slices.Equal(got, want) {
		t.Errorf("with external-status-address: status %v, want %v", got, want)
	}

	su.ClearStatusFromExternalService()
	su.SaveStatusFromExternalStatus("")
	if got, want := statusAddresses(su), []string{"10.0.0.1", "10.0.0.2"}; !slices.Equal(got, want) {
		t.Errorf("without either: status %v, want the nodes %v", got, want)
	}
}

func TestControllerNodesLeaveOutStoppingPods(t *testing.T) {
	t.Parallel()
	node := func(n string) *string { return &n }
	yes, no := true, false
	slices_ := []*discovery_v1.EndpointSlice{
		{Endpoints: []discovery_v1.Endpoint{
			{NodeName: node("b"), Conditions: discovery_v1.EndpointConditions{Ready: &yes}},
			{NodeName: node("a"), Conditions: discovery_v1.EndpointConditions{Ready: &no}}, // starting
			{NodeName: node("c"), Conditions: discovery_v1.EndpointConditions{Ready: &no, Terminating: &yes}},
		}},
		{Endpoints: []discovery_v1.Endpoint{
			{NodeName: node("b")},
			{},
		}},
	}
	if got, want := controllerNodes(slices_), []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("nodes %v, want %v", got, want)
	}
}

func TestNodeAddressesAreTheExternalOrElseTheInternalIP(t *testing.T) {
	t.Parallel()
	nodeWith := func(name string, addrs ...api_v1.NodeAddress) *api_v1.Node {
		return &api_v1.Node{ObjectMeta: meta_v1.ObjectMeta{Name: name}, Status: api_v1.NodeStatus{Addresses: addrs}}
	}
	client := fake.NewSimpleClientset(
		nodeWith("internal", api_v1.NodeAddress{Type: api_v1.NodeHostName, Address: "internal"}, api_v1.NodeAddress{Type: api_v1.NodeInternalIP, Address: "10.0.0.2"}),
		nodeWith("external", api_v1.NodeAddress{Type: api_v1.NodeInternalIP, Address: "10.0.0.1"}, api_v1.NodeAddress{Type: api_v1.NodeExternalIP, Address: "203.0.113.1"}),
		nodeWith("same", api_v1.NodeAddress{Type: api_v1.NodeInternalIP, Address: "10.0.0.2"}),
	)
	lbc := &LoadBalancerController{client: client, ctx: context.Background(), Logger: quietLogger()}

	got, ok := lbc.nodeAddressesOf([]string{"external", "gone", "internal", "same"})
	if !ok {
		t.Fatal("the nodes were not listed")
	}
	if want := []string{"10.0.0.2", "203.0.113.1"}; !slices.Equal(got, want) {
		t.Errorf("addresses %v, want %v", got, want)
	}
}

// When the nodes cannot be listed, the status stays as it is instead of
// losing every address.
func TestNodeAddressesThatCannotBeListedKeepTheStatus(t *testing.T) {
	t.Parallel()
	client := fake.NewSimpleClientset()
	client.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the API server is away")
	})
	lbc := &LoadBalancerController{client: client, ctx: context.Background(), Logger: quietLogger()}
	if _, ok := lbc.nodeAddressesOf([]string{"a"}); ok {
		t.Error("a failed list gave addresses")
	}
}
