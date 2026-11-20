package k8s

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// referencedConfigMapSyncTimeout is how long the first look at a ConfigMap
// waits for its informer: the Ingress that names it is configured with it,
// not first without.
const referencedConfigMapSyncTimeout = 5 * time.Second

// referencedConfigMaps watches the ConfigMaps resources name - those of
// ingress-nginx's custom-headers and auth-proxy-set-headers, its tcp-services
// and udp-services - each with an informer of its own for just that
// ConfigMap, started when something names it first and stopped when nothing
// names it any more. Watching every ConfigMap of the cluster, as ingress-nginx
// does, would hold all of them in memory for the few that are named.
//
// A change of a watched ConfigMap is handed to changed, which puts it on the
// sync queue: the configuration is touched by the sync loop only.
type referencedConfigMaps struct {
	client  kubernetes.Interface
	resync  time.Duration
	changed func(cm *v1.ConfigMap)

	mu sync.Mutex
	// watches are the informers by namespace/name
	watches map[string]*configMapWatch
	// users are, per user (an Ingress's key, "tcp-services", ...), the ConfigMaps it names
	users map[string][]string
}

type configMapWatch struct {
	store  cache.Store
	ctrl   cache.Controller
	stop   chan struct{}
	synced bool
}

func newReferencedConfigMaps(client kubernetes.Interface, resync time.Duration, changed func(cm *v1.ConfigMap)) *referencedConfigMaps {
	return &referencedConfigMaps{
		client:  client,
		resync:  resync,
		changed: changed,
		watches: map[string]*configMapWatch{},
		users:   map[string][]string{},
	}
}

// use says which ConfigMaps user names now and returns them, nil for one
// that does not exist. Informers nothing names any more stop.
func (r *referencedConfigMaps) use(ctx context.Context, user string, keys []string) map[string]*v1.ConfigMap {
	r.mu.Lock()
	if len(keys) == 0 {
		delete(r.users, user)
	} else {
		r.users[user] = slices.Clone(keys)
	}
	var started []*configMapWatch
	for _, key := range keys {
		if _, ok := r.watches[key]; !ok {
			w := r.start(key)
			r.watches[key] = w
			started = append(started, w)
		}
	}
	r.stopUnused()
	r.mu.Unlock()

	for _, w := range started {
		waitCtx, cancel := context.WithTimeout(ctx, referencedConfigMapSyncTimeout)
		cache.WaitForCacheSync(waitCtx.Done(), w.ctrl.HasSynced)
		cancel()
		r.mu.Lock()
		w.synced = true
		r.mu.Unlock()
	}

	out := make(map[string]*v1.ConfigMap, len(keys))
	for _, key := range keys {
		out[key] = r.get(key)
	}
	return out
}

// get is the ConfigMap, nil if it does not exist or is not watched.
func (r *referencedConfigMaps) get(key string) *v1.ConfigMap {
	r.mu.Lock()
	w := r.watches[key]
	r.mu.Unlock()
	if w == nil {
		return nil
	}
	obj, exists, err := w.store.GetByKey(key)
	if err != nil || !exists {
		return nil
	}
	cm, _ := obj.(*v1.ConfigMap)
	return cm
}

// usersOf are the users that name key.
func (r *referencedConfigMaps) usersOf(key string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for user, keys := range r.users {
		if slices.Contains(keys, key) {
			out = append(out, user)
		}
	}
	slices.Sort(out)
	return out
}

// watched tells whether key is a ConfigMap something names.
func (r *referencedConfigMaps) watched(key string) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.watches[key]
	return ok
}

// stopAll stops every informer.
func (r *referencedConfigMaps) stopAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.users = map[string][]string{}
	r.stopUnused()
}

func (r *referencedConfigMaps) stopUnused() {
	named := map[string]bool{}
	for _, keys := range r.users {
		for _, key := range keys {
			named[key] = true
		}
	}
	for _, key := range slices.Collect(maps.Keys(r.watches)) {
		if !named[key] {
			close(r.watches[key].stop)
			delete(r.watches, key)
		}
	}
}

func (r *referencedConfigMaps) start(key string) *configMapWatch {
	ns, name, _ := cache.SplitMetaNamespaceKey(key)
	w := &configMapWatch{stop: make(chan struct{})}
	selector := fields.OneTermEqualSelector("metadata.name", name).String()
	notify := func(obj interface{}) {
		if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
			obj = d.Obj
		}
		cm, ok := obj.(*v1.ConfigMap)
		if !ok || cm.Namespace != ns || cm.Name != name {
			// the field selector keeps others out; a watch that does not take it (fake clients) does not
			return
		}
		r.mu.Lock()
		synced := w.synced
		r.mu.Unlock()
		// what the first list finds is what use returns; the rest is a change
		if synced && r.changed != nil {
			r.changed(cm)
		}
	}
	w.store, w.ctrl = cache.NewInformerWithOptions(cache.InformerOptions{
		ListerWatcher: cache.ToListWatcherWithWatchListSemantics(&cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
				options.FieldSelector = selector
				return r.client.CoreV1().ConfigMaps(ns).List(context.Background(), options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.FieldSelector = selector
				return r.client.CoreV1().ConfigMaps(ns).Watch(context.Background(), options)
			},
		}, r.client),
		ObjectType:   &v1.ConfigMap{},
		ResyncPeriod: r.resync,
		Handler: cache.ResourceEventHandlerFuncs{
			AddFunc: notify,
			UpdateFunc: func(old, cur interface{}) {
				if oldCM, ok := old.(*v1.ConfigMap); ok {
					if curCM, ok := cur.(*v1.ConfigMap); ok && oldCM.ResourceVersion == curCM.ResourceVersion {
						return
					}
				}
				notify(cur)
			},
			DeleteFunc: notify,
		},
	})
	go w.ctrl.Run(w.stop)
	return w
}

// useConfigMaps says which ConfigMaps user names now and returns them.
func (lbc *LoadBalancerController) useConfigMaps(user string, keys []string) map[string]*v1.ConfigMap {
	if lbc.referencedConfigMaps == nil {
		return nil
	}
	ctx := lbc.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if lbc.admission.checking {
		// an Ingress the webhook checks is not a user yet: the ConfigMaps watched already, or none
		out := make(map[string]*v1.ConfigMap, len(keys))
		for _, key := range keys {
			out[key] = lbc.referencedConfigMaps.get(key)
		}
		if len(keys) == 0 {
			return nil
		}
		return out
	}
	cms := lbc.referencedConfigMaps.use(ctx, user, keys)
	if len(keys) == 0 {
		return nil
	}
	return cms
}

// syncReferencedConfigMap configures again what names the ConfigMap key:
// the Ingresses of custom-headers and auth-proxy-set-headers (together with
// those of their host they are merged with), and the tcp/udp services.
func (lbc *LoadBalancerController) syncReferencedConfigMap(key string) {
	users := lbc.referencedConfigMaps.usersOf(key)
	ingresses := map[string]bool{}
	for _, user := range users {
		if handler, ok := lbc.configMapUsers[user]; ok {
			handler()
			continue
		}
		ingresses[user] = true
	}
	if len(ingresses) == 0 {
		return
	}
	var resources []Resource
	for _, r := range lbc.configuration.GetResourcesWithFilter(resourceFilter{Ingresses: true}) {
		ic, ok := r.(*IngressConfiguration)
		if !ok {
			continue
		}
		if slices.ContainsFunc(ingressesOfConfiguration(ic), func(k string) bool { return ingresses[k] }) {
			resources = append(resources, r)
		}
	}
	if len(resources) == 0 {
		return
	}
	resourceExes := lbc.createExtendedResources(resources)
	warnings, updateErr := lbc.configurator.AddOrUpdateResources(resourceExes, true)
	lbc.updateResourcesStatusAndEvents(mergeExtendedResourceWarnings(resources, resourceExes), warnings, updateErr)
}

// ingressesOfConfiguration are the keys of the Ingresses an IngressConfiguration is made of.
func ingressesOfConfiguration(ic *IngressConfiguration) []string {
	keys := []string{ic.Ingress.Namespace + "/" + ic.Ingress.Name}
	for _, m := range ic.Minions {
		keys = append(keys, m.Ingress.Namespace+"/"+m.Ingress.Name)
	}
	for _, m := range ic.Merged {
		keys = append(keys, m.Ingress.Namespace+"/"+m.Ingress.Name)
	}
	for _, c := range ic.Canaries {
		keys = append(keys, c.Namespace+"/"+c.Name)
	}
	return keys
}
