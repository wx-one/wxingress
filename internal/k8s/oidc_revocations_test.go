package k8s

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wx-one/wxingress/internal/nginx"
	api_v1 "k8s.io/api/core/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeRevocationsModule struct {
	set   nginx.OIDCRevocations
	local []nginx.OIDCRevocation
}

func (f *fakeRevocationsModule) SetOIDCRevocations(r nginx.OIDCRevocations) error {
	f.set = r
	return nil
}
func (f *fakeRevocationsModule) LocalOIDCRevocations() ([]nginx.OIDCRevocation, error) {
	return f.local, nil
}

func TestOIDCRevocationsRoundTrip(t *testing.T) {
	t.Parallel()
	r, wrong := ParseOIDCRevocations("# a comment\nall 100\nsub alice 200\nsid s-1 300\nbogus line\n")
	if r.All != 100 || len(r.Entries) != 2 || len(wrong) != 1 {
		t.Fatalf("parsed %+v, wrong %q", r, wrong)
	}
	now := time.Unix(1000, 0)
	if got := FormatOIDCRevocations(r, now); got != "all 100\nsid s-1 300\nsub alice 200\n" {
		t.Errorf("formatted %q", got)
	}
	old := nginx.OIDCRevocations{Entries: []nginx.OIDCRevocation{{Key: "sub:bob", Before: 1}}}
	if got := FormatOIDCRevocations(old, time.Unix(1, 0).Add(31*24*time.Hour)); got != "\n" {
		t.Errorf("a line older than a month stays: %q", got)
	}
}

func TestOIDCRevocationsSync(t *testing.T) {
	t.Parallel()
	now := time.Now()
	client := fake.NewSimpleClientset(&api_v1.ConfigMap{
		ObjectMeta: meta_v1.ObjectMeta{Name: "rev", Namespace: "ns", ResourceVersion: "1"},
		Data:       map[string]string{OIDCRevocationsKey: fmt.Sprintf("sub alice %d\n", now.Unix()-10)},
	})
	module := &fakeRevocationsModule{local: []nginx.OIDCRevocation{{Key: "sid:s-9", Before: now.Unix(), Local: true}}}
	if err := syncOIDCRevocations(context.Background(), client, "ns", "rev", module, now); err != nil {
		t.Fatal(err)
	}
	cm, _ := client.CoreV1().ConfigMaps("ns").Get(context.Background(), "rev", meta_v1.GetOptions{})
	got, _ := ParseOIDCRevocations(cm.Data[OIDCRevocationsKey])
	if len(got.Entries) != 2 || len(module.set.Entries) != 2 {
		t.Errorf("the local logout is not in the ConfigMap and the module: %q, %+v", cm.Data[OIDCRevocationsKey], module.set)
	}

	// no ConfigMap yet: made by the first logout
	empty := fake.NewSimpleClientset()
	if err := syncOIDCRevocations(context.Background(), empty, "ns", "rev", module, now); err != nil {
		t.Fatal(err)
	}
	if cm, err := empty.CoreV1().ConfigMaps("ns").Get(context.Background(), "rev", meta_v1.GetOptions{}); err != nil || cm.Data[OIDCRevocationsKey] == "" {
		t.Errorf("no ConfigMap made: %v", err)
	}
}
