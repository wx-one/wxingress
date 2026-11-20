package k8s

import (
	"context"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestReferencedConfigMaps(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset(&v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "headers"}, Data: map[string]string{"X-A": "1"}})
	var mu sync.Mutex
	var changed []string
	r := newReferencedConfigMaps(client, 0, func(cm *v1.ConfigMap) {
		mu.Lock()
		defer mu.Unlock()
		changed = append(changed, cm.Namespace+"/"+cm.Name+"="+cm.Data["X-A"])
	})
	defer r.stopAll()
	ctx := context.Background()

	got := r.use(ctx, "app/ing", []string{"app/headers", "app/missing"})
	if cm := got["app/headers"]; cm == nil || cm.Data["X-A"] != "1" {
		t.Fatalf("headers %v", cm)
	}
	if got["app/missing"] != nil {
		t.Errorf("missing %v", got["app/missing"])
	}
	if !r.watched("app/headers") || len(r.usersOf("app/headers")) != 1 {
		t.Errorf("watched %v users %v", r.watched("app/headers"), r.usersOf("app/headers"))
	}
	// the first list is no change
	mu.Lock()
	if len(changed) != 0 {
		t.Errorf("changes of the first list: %v", changed)
	}
	mu.Unlock()

	cm, _ := client.CoreV1().ConfigMaps("app").Get(ctx, "headers", metav1.GetOptions{})
	cm.Data["X-A"] = "2"
	cm.ResourceVersion = "2"
	if _, err := client.CoreV1().ConfigMaps("app").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().ConfigMaps("app").Create(ctx, &v1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "app", Name: "missing"}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(changed)
		mu.Unlock()
		if n >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	if len(changed) != 2 {
		t.Errorf("changes %v", changed)
	}
	mu.Unlock()
	if cm := r.get("app/headers"); cm == nil || cm.Data["X-A"] != "2" {
		t.Errorf("after the update %v", cm)
	}

	// another user keeps one watched; nothing names the other any more
	r.use(ctx, "app/other", []string{"app/headers"})
	r.use(ctx, "app/ing", nil)
	if !r.watched("app/headers") || r.watched("app/missing") {
		t.Errorf("watched headers %v, missing %v", r.watched("app/headers"), r.watched("app/missing"))
	}
	r.use(ctx, "app/other", nil)
	if r.watched("app/headers") {
		t.Error("headers is still watched")
	}
}
