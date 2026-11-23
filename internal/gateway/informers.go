package gateway

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	corelisters "k8s.io/client-go/listers/core/v1"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	gwinformers "sigs.k8s.io/gateway-api/pkg/client/informers/externalversions"
	gwlisters "sigs.k8s.io/gateway-api/pkg/client/listers/apis/v1"
)

// What the Gateway layer reads, and from where:
//
//   - Services and Secrets of the watched namespaces: the controller's own
//     informers (Sources), which -watch-namespace, -watch-namespace-label and
//     -watch-secret-namespace already shape. Their changes reach the Gateway
//     layer through the controller's queue.
//   - The routes, Gateways and ReferenceGrants: informers of the Gateway API
//     per watched namespace, made, started and stopped with the controller's
//     own per namespace (WatchNamespace, UnwatchNamespace).
//   - GatewayClasses, which have no namespace, and Namespaces, whose labels
//     the routes' allowedRoutes select by: one informer each.
//   - The EndpointSlices of the controller's namespace - those of the
//     controller's own Service, which the Gateways' Services take - an
//     informer of that namespace alone, which need not be watched.
//   - The Services of the Gateways of the class, in every namespace: an
//     informer of the cluster that lists only Services with the class's
//     labels. A namespace that loses its label leaves its Gateways' Services
//     behind, and the leader must see them to delete them (collect) - else
//     they keep pointing at nginx ports another Gateway gets next.

// Sources are the controller's listers the Gateway layer reads.
type Sources interface {
	// ListServices lists the Services of the watched namespaces.
	ListServices() []*v1.Service
	// ListSecrets lists the Secrets of the namespaces Secrets are watched in.
	ListSecrets() []*v1.Secret
}

// nsWatch are the Gateway API informers of one namespace.
type nsWatch struct {
	factory gwinformers.SharedInformerFactory
	stop    chan struct{}

	gateways        gwlisters.GatewayLister
	httpRoutes      gwlisters.HTTPRouteLister
	grpcRoutes      gwlisters.GRPCRouteLister
	tlsRoutes       gwlisters.TLSRouteLister
	tcpRoutes       gwlisters.TCPRouteLister
	udpRoutes       gwlisters.UDPRouteLister
	referenceGrants gwlisters.ReferenceGrantLister
	// listenerSets is nil where the cluster has no ListenerSet CRD
	listenerSets gwlisters.ListenerSetLister

	informers []cache.SharedIndexInformer
}

func (c *Controller) newNSWatch(ns string) *nsWatch {
	f := gwinformers.NewSharedInformerFactoryWithOptions(c.cfg.GatewayClient, c.cfg.Resync, gwinformers.WithNamespace(ns))
	gw := f.Gateway().V1()
	w := &nsWatch{
		factory:         f,
		stop:            make(chan struct{}),
		gateways:        gw.Gateways().Lister(),
		httpRoutes:      gw.HTTPRoutes().Lister(),
		grpcRoutes:      gw.GRPCRoutes().Lister(),
		tlsRoutes:       gw.TLSRoutes().Lister(),
		tcpRoutes:       gw.TCPRoutes().Lister(),
		udpRoutes:       gw.UDPRoutes().Lister(),
		referenceGrants: gw.ReferenceGrants().Lister(),
	}
	w.informers = []cache.SharedIndexInformer{
		gw.Gateways().Informer(),
		gw.HTTPRoutes().Informer(),
		gw.GRPCRoutes().Informer(),
		gw.TLSRoutes().Informer(),
		gw.TCPRoutes().Informer(),
		gw.UDPRoutes().Informer(),
		gw.ReferenceGrants().Informer(),
	}
	if c.cfg.ListenerSets {
		w.listenerSets = gw.ListenerSets().Lister()
		w.informers = append(w.informers, gw.ListenerSets().Informer())
	}
	return w
}

// watches holds the informers of the watched namespaces.
type watches struct {
	mu sync.RWMutex
	// by namespace; "" is all of them
	byNamespace map[string]*nsWatch
	// started is the stop channel of Start, nil before
	started <-chan struct{}
	// onChange is what every change calls, nil before OnChange
	onChange func()
}

// WatchNamespace starts watching the Gateway API resources of ns ("" for
// all), as the controller does when it starts watching a namespace.
func (c *Controller) WatchNamespace(ns string) {
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	if _, ok := c.watches.byNamespace[ns]; ok {
		return
	}
	w := c.newNSWatch(ns)
	c.watches.byNamespace[ns] = w
	if c.watches.onChange != nil {
		if err := addHandler(w.informers, c.watches.onChange); err != nil {
			c.logger.Error(err, "watching the Gateway API", "namespace", ns)
		}
	}
	if c.watches.started != nil {
		c.startWatch(w)
	}
}

// UnwatchNamespace stops watching the Gateway API resources of ns, as the
// controller does when a namespace loses its label.
func (c *Controller) UnwatchNamespace(ns string) {
	c.watches.mu.Lock()
	w, ok := c.watches.byNamespace[ns]
	delete(c.watches.byNamespace, ns)
	onChange := c.watches.onChange
	c.watches.mu.Unlock()
	if !ok {
		return
	}
	close(w.stop)
	if onChange != nil {
		onChange()
	}
}

// startWatch starts the informers of a namespace, and once they have
// synced asks for a Build: a namespace watched later must not wait for the
// next change.
func (c *Controller) startWatch(w *nsWatch) {
	stop := make(chan struct{})
	go func() {
		select {
		case <-c.watches.started:
		case <-w.stop:
		}
		close(stop)
	}()
	w.factory.Start(stop)
	onChange := c.watches.onChange
	go func() {
		synced := make([]cache.InformerSynced, 0, len(w.informers))
		for _, inf := range w.informers {
			synced = append(synced, inf.HasSynced)
		}
		if cache.WaitForCacheSync(stop, synced...) && onChange != nil {
			onChange()
		}
	}()
}

// sortedWatches are the informers of the watched namespaces, in the order
// of their namespaces.
func (c *Controller) sortedWatches() []*nsWatch {
	c.watches.mu.RLock()
	defer c.watches.mu.RUnlock()
	out := make([]*nsWatch, 0, len(c.watches.byNamespace))
	for _, ns := range slices.Sorted(maps.Keys(c.watches.byNamespace)) {
		out = append(out, c.watches.byNamespace[ns])
	}
	return out
}

// watchOf is the watch that has the resources of ns, nil if ns is not watched.
func (c *Controller) watchOf(ns string) *nsWatch {
	c.watches.mu.RLock()
	defer c.watches.mu.RUnlock()
	if w, ok := c.watches.byNamespace[""]; ok {
		return w
	}
	return c.watches.byNamespace[ns]
}

func addHandler(infs []cache.SharedIndexInformer, fn func()) error {
	h := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { fn() },
		UpdateFunc: func(any, any) { fn() },
		DeleteFunc: func(any) { fn() },
	}
	for _, inf := range infs {
		if _, err := inf.AddEventHandler(h); err != nil {
			return fmt.Errorf("adding a Gateway API event handler: %w", err)
		}
	}
	return nil
}

// clusterInformers are the informers of things with no namespace or of the
// controller's namespace.
type clusterInformers struct {
	classFactory gwinformers.SharedInformerFactory
	nsFactory    informers.SharedInformerFactory
	ownFactory   informers.SharedInformerFactory
	// svcFactory lists the Services of the class's Gateways, in all namespaces
	svcFactory informers.SharedInformerFactory

	gatewayClasses gwlisters.GatewayClassLister
	namespaces     corelisters.NamespaceLister
	// services are the Services of the Gateways of the class, in all
	// namespaces
	services corelisters.ServiceLister
	// endpointSlices are those of the controller's namespace: those of its
	// own Service go into the EndpointSlices of the Gateways' Services
	endpointSlices discoverylisters.EndpointSliceLister
}

func (c *Controller) newClusterInformers() {
	c.classFactory = gwinformers.NewSharedInformerFactory(c.cfg.GatewayClient, c.cfg.Resync)
	c.nsFactory = informers.NewSharedInformerFactory(c.cfg.KubeClient, c.cfg.Resync)
	c.ownFactory = informers.NewSharedInformerFactoryWithOptions(c.cfg.KubeClient, c.cfg.Resync, informers.WithNamespace(c.cfg.Namespace))
	c.gatewayClasses = c.classFactory.Gateway().V1().GatewayClasses().Lister()
	c.namespaces = c.nsFactory.Core().V1().Namespaces().Lister()
	c.svcFactory = informers.NewSharedInformerFactoryWithOptions(c.cfg.KubeClient, c.cfg.Resync,
		informers.WithTweakListOptions(func(o *metav1.ListOptions) {
			o.LabelSelector = c.ownServiceSelector().String()
		}))
	c.services = c.svcFactory.Core().V1().Services().Lister()
	c.endpointSlices = c.ownFactory.Discovery().V1().EndpointSlices().Lister()
}

func (c *Controller) clusterInformerList() []cache.SharedIndexInformer {
	return []cache.SharedIndexInformer{
		c.classFactory.Gateway().V1().GatewayClasses().Informer(),
		c.nsFactory.Core().V1().Namespaces().Informer(),
		c.svcFactory.Core().V1().Services().Informer(),
		c.ownFactory.Discovery().V1().EndpointSlices().Informer(),
	}
}

// ownServiceSelector selects the Services of the Gateways of the class.
func (c *Controller) ownServiceSelector() labels.Selector {
	return labels.SelectorFromSet(labels.Set{LabelManagedBy: managedBy, LabelGatewayClass: c.cfg.GatewayClass})
}

// syncedFor tells whether the Gateways of ns are known: ns is not watched
// (it has none of the class to serve), or its informers have synced. A
// namespace that gets the label is watched before its informers have listed
// its Gateways, and a Build in between must not take their Services for
// left behind.
func (c *Controller) syncedFor(ns string) bool {
	w := c.watchOf(ns)
	if w == nil {
		return true
	}
	for _, inf := range w.informers {
		if !inf.HasSynced() {
			return false
		}
	}
	return true
}
