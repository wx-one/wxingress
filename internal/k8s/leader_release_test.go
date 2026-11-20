package k8s

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/leaderelection"
)

// TestLeaderGivesUpTheLeaseWhenItStops: the function runLeaderElector returns
// leaves the Lease without a holder, so that the next leader takes it at once
// instead of after its 30 seconds - and does not make the Lease anew when
// helm deleted it, as the leader election of a running pod would.
//
//nolint:paralleltest // t.Setenv(POD_NAME), which newLeaderElector reads
func TestLeaderGivesUpTheLeaseWhenItStops(t *testing.T) {
	t.Setenv("POD_NAME", "pod-a")
	client := fake.NewClientset()
	le, err := newLeaderElector(client, leaderelection.LeaderCallbacks{
		OnStartedLeading: func(context.Context) {},
		OnStoppedLeading: func() {},
	}, "wxingress", "lock")
	if err != nil {
		t.Fatal(err)
	}
	stop := runLeaderElector(le)
	deadline := time.Now().Add(5 * time.Second)
	for !le.IsLeader() {
		if time.Now().After(deadline) {
			stop()
			t.Fatal("not the leader within 5s")
		}
		time.Sleep(10 * time.Millisecond)
	}
	currentHolder := func() string {
		l, err := client.CoordinationV1().Leases("wxingress").Get(context.Background(), "lock", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return leaseHolder(l)
	}
	if h := currentHolder(); h != "pod-a" {
		t.Fatalf("holder %q while leading", h)
	}

	start := time.Now()
	stop()
	if waited := time.Since(start); waited >= releaseTimeout {
		t.Errorf("stopping took %v, the release timeout", waited)
	}
	if h := currentHolder(); h != "" {
		t.Errorf("holder %q after stopping, want none", h)
	}
	if le.IsLeader() {
		t.Error("still the leader after stopping")
	}
}

func leaseHolder(l *coordinationv1.Lease) string {
	if l.Spec.HolderIdentity == nil {
		return ""
	}
	return *l.Spec.HolderIdentity
}
