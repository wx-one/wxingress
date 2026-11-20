package k8s

import (
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/wx-one/wxingress/pkg/apis/dos/v1beta1"

	"github.com/wx-one/wxingress/internal/k8s/appprotect"
	"github.com/wx-one/wxingress/internal/k8s/appprotectdos"
	nl "github.com/wx-one/wxingress/internal/logger"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
	v1 "k8s.io/api/core/v1"
	discovery_v1 "k8s.io/api/discovery/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"
)

// taskQueue manages a work queue through an independent worker that
// invokes the given sync function for every work item inserted.
type taskQueue struct {
	// queue is the work queue the worker polls
	queue *workqueue.Type
	// sync is called for each item in the queue
	sync func(task)
	// workerDone is closed when the worker exits
	workerDone chan struct{}
	// logger
	logger *slog.Logger
	// coalesced counts the coalesced tasks in the queue, which are no events of their own
	coalesced atomic.Int32
}

// newTaskQueue creates a new task queue with the given sync function.
// The sync function is called for every element inserted into the queue.
func newTaskQueue(logger *slog.Logger, syncFn func(task)) *taskQueue {
	return &taskQueue{
		queue:      workqueue.NewNamed("taskQueue"),
		sync:       syncFn,
		workerDone: make(chan struct{}),
		logger:     logger,
	}
}

// Run begins running the worker for the given duration
func (tq *taskQueue) Run(period time.Duration, stopCh <-chan struct{}) {
	wait.Until(tq.worker, period, stopCh)
}

// Enqueue enqueues ns/name of the given api object in the task queue.
func (tq *taskQueue) Enqueue(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		nl.Debugf(tq.logger, "Couldn't get key for object %v: %v", obj, err)
		return
	}

	task, err := newTask(key, obj)
	if err != nil {
		nl.Debugf(tq.logger, "Couldn't create a task for object %v: %v", obj, err)
		return
	}

	nl.Debugf(tq.logger, "Adding an element with a key: %v", task.Key)
	tq.queue.Add(task)
}

// Add adds a task to the queue.
func (tq *taskQueue) Add(t task) {
	tq.queue.Add(t)
}

// Requeue adds the task to the queue again and logs the given error
func (tq *taskQueue) Requeue(task task, err error) {
	nl.Errorf(tq.logger, "Requeuing %v, err %v", task.Key, err)
	tq.queue.Add(task)
}

// Len returns the length of the queue
func (tq *taskQueue) Len() int {
	nl.Debugf(tq.logger, "The queue has %v element(s)", tq.queue.Len())
	return tq.queue.Len()
}

// RequeueAfter adds the task to the queue after the given duration
func (tq *taskQueue) RequeueAfter(t task, err error, after time.Duration) {
	nl.Errorf(tq.logger, "Requeuing %v after %s, err %v", t.Key, after.String(), err)
	go func(t task, after time.Duration) {
		time.Sleep(after)
		tq.queue.Add(t)
	}(t, after)
}

// Worker processes work in the queue through sync.
// coalescedTask is a task that stands for any number of events: however many
// ask for it, it is in the queue once, and it runs at most once per interval,
// as ingress-nginx's sync queue runs one sync for all events that came since
// the last, at most --sync-rate-limit times a second. Its fields belong to the
// worker: it is scheduled and started from tasks.
type coalescedTask struct {
	task      task
	interval  time.Duration
	scheduled bool
	started   time.Time
}

// Schedule queues a coalesced task unless it is already on its way: at once
// if it last started more than its interval ago, and otherwise when the
// interval is over.
func (tq *taskQueue) Schedule(c *coalescedTask) {
	if c.scheduled {
		return
	}
	c.scheduled = true
	// the task as it is now: a timer adds it later, from another goroutine,
	// while the worker may set the fields of c again
	t := c.task
	add := func() {
		tq.coalesced.Add(1)
		tq.queue.Add(t)
	}
	if wait := time.Until(c.started.Add(c.interval)); wait > 0 {
		time.AfterFunc(wait, add)
		return
	}
	add()
}

// Started tells that a coalesced task was taken from the queue: what asks for
// it from now on schedules it again.
func (tq *taskQueue) Started(c *coalescedTask) {
	c.scheduled = false
	c.started = time.Now()
	tq.coalesced.Add(-1)
}

// Pending is the number of events in the queue, without the coalesced tasks:
// a coalesced task waiting in the queue makes no batch.
func (tq *taskQueue) Pending() int {
	return max(tq.queue.Len()-int(tq.coalesced.Load()), 0)
}

func (tq *taskQueue) worker() {
	for {
		t, quit := tq.queue.Get()
		if quit {
			close(tq.workerDone)
			return
		}
		nl.Debugf(tq.logger, "Syncing %v", t.(task).Key)
		tq.sync(t.(task))
		tq.queue.Done(t)
	}
}

// Shutdown shuts down the work queue and waits for the worker to ACK
func (tq *taskQueue) Shutdown() {
	tq.queue.ShutDown()
	<-tq.workerDone
}

// kind represents the kind of the Kubernetes resources of a task
type kind int

// resources
const (
	ingress = iota
	endpointslice
	configMap
	secret
	service
	namespace
	virtualserver
	virtualServerRoute
	globalConfiguration
	transportserver
	policy
	appProtectPolicy
	appProtectLogConf
	appProtectUserSig
	appProtectDosPolicy
	appProtectDosLogConf
	appProtectDosProtectedResource
	ingressLink
	// certificates gives the wxingress module the certificates of hosts that changed since it was last given them
	certificates
	// serviceEndpoints is a Service whose EndpointSlices changed (-dynamic-upstreams)
	serviceEndpoints
	// endpoints gives the wxingress module the servers of the Services whose EndpointSlices changed since it last ran
	endpoints
	// reload reloads NGINX with the changes that waited for the end of the reload interval (-dynamic-upstreams)
	reload
	// retry gives the wxingress module again the canary rules and certificates it did not take (-dynamic-upstreams)
	retry
	// gatewayAPI evaluates all Gateway API resources (-enable-gateway-api)
	gatewayAPI
)

// task is an element of a taskQueue
type task struct {
	Kind kind
	Key  string
}

// newTask creates a new task
func newTask(key string, obj interface{}) (task, error) {
	var k kind
	switch t := obj.(type) {
	case *networking.Ingress:
		k = ingress
	case *discovery_v1.EndpointSlice:
		k = endpointslice
	case *v1.ConfigMap:
		k = configMap
	case *v1.Secret:
		k = secret
	case *v1.Service:
		k = service
	case *v1.Namespace:
		k = namespace
	case *conf_v1.VirtualServer:
		k = virtualserver
	case *conf_v1.VirtualServerRoute:
		k = virtualServerRoute
	case *conf_v1.Policy:
		k = policy
	case *conf_v1.GlobalConfiguration:
		k = globalConfiguration
	case *conf_v1.TransportServer:
		k = transportserver
	case *v1beta1.DosProtectedResource:
		k = appProtectDosProtectedResource
	case *unstructured.Unstructured:
		if objectKind := obj.(*unstructured.Unstructured).GetKind(); objectKind == appprotect.PolicyGVK.Kind {
			k = appProtectPolicy
		} else if objectKind == appprotect.LogConfGVK.Kind {
			k = appProtectLogConf
		} else if objectKind == ingressLinkGVK.Kind {
			k = ingressLink
		} else if objectKind == appprotect.UserSigGVK.Kind {
			k = appProtectUserSig
		} else if objectKind == appprotectdos.DosPolicyGVK.Kind {
			k = appProtectDosPolicy
		} else if objectKind == appprotectdos.DosLogConfGVK.Kind {
			k = appProtectDosLogConf
		} else {
			return task{}, fmt.Errorf("unknown unstructured kind: %v", objectKind)
		}
	default:
		return task{}, fmt.Errorf("unknown type: %v", t)
	}

	return task{k, key}, nil
}
