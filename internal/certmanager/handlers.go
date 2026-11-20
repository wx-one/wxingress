package certmanager

import (
	"reflect"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

// The three helpers of cert-manager's pkg/controller this controller used,
// said here: that package brings all of k8s.io/apiserver with it - CEL, the
// storage cacher, gRPC and OpenTelemetry - which only made the binary, and
// what of it the controller keeps in memory, bigger.

// itemBasedRateLimiter waits 5s after an item's first failure, doubling up to 5 minutes.
func itemBasedRateLimiter() workqueue.TypedRateLimiter[types.NamespacedName] {
	return workqueue.NewTypedItemExponentialFailureRateLimiter[types.NamespacedName](5*time.Second, 5*time.Minute)
}

// queuingEventHandler queues the name of every object added, changed or deleted.
func queuingEventHandler(queue workqueue.TypedRateLimitingInterface[types.NamespacedName]) cache.ResourceEventHandler {
	return blockingEventHandler(func(obj metav1.Object) {
		queue.Add(cache.MetaObjectToName(obj).AsNamespacedName())
	})
}

// blockingEventHandler calls work with every object added, changed or
// deleted, in the informer's goroutine. An update that changed nothing - a
// resync - is skipped: same resourceVersion, or, where there is none, equal
// objects.
func blockingEventHandler[T metav1.Object](work func(T)) cache.ResourceEventHandler {
	run := func(obj any) {
		if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = tombstone.Obj
		}
		if t, ok := obj.(T); ok {
			work(t)
		}
	}
	return cache.ResourceEventHandlerFuncs{
		AddFunc: run,
		UpdateFunc: func(oldObj, newObj any) {
			o, okOld := oldObj.(T)
			n, okNew := newObj.(T)
			if !okOld || !okNew {
				return
			}
			if o.GetResourceVersion() != "" || n.GetResourceVersion() != "" {
				if o.GetResourceVersion() == n.GetResourceVersion() {
					return
				}
			} else if reflect.DeepEqual(oldObj, newObj) {
				return
			}
			work(n)
		},
		DeleteFunc: run,
	}
}
