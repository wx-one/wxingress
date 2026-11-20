package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"time"

	"github.com/go-logr/logr"
	v1 "k8s.io/api/core/v1"
	discoveryV1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwclient "sigs.k8s.io/gateway-api/pkg/client/clientset/versioned"

	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"

	ngfAPIv1alpha2 "github.com/wx-one/wxingress/internal/third_party/ngf/apis/v1alpha2"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/nginx/config/policies"
	ngxvalidation "github.com/wx-one/wxingress/internal/third_party/ngf/controller/nginx/config/validation"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/conditions"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/dataplane"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/resolver"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/validation"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/status"
	"github.com/wx-one/wxingress/internal/third_party/ngf/framework/waf/fetch"
)

// DefaultControllerName is the controllerName a GatewayClass names to be
// served by wxingress.
const DefaultControllerName = "wxingress.wizardtales.com/gateway-controller"

// DefaultGatewayClass is the name of the GatewayClass wxingress serves by
// default.
const DefaultGatewayClass = "wxingress"

// Config configures a Controller.
type Config struct {
	KubeClient    kubernetes.Interface
	GatewayClient gwclient.Interface
	Logger        *slog.Logger
	// ControllerName is the spec.controllerName of the GatewayClass to serve.
	ControllerName string
	// GatewayClass is the name of the GatewayClass to serve; Gateways of other
	// classes are ignored.
	GatewayClass string
	// IngressClass is the class of the VirtualServers made from Gateways, so
	// that the controller takes them as its own.
	IngressClass string
	// HTTPPort and HTTPSPort are the ports nginx listens on for HTTP and
	// HTTPS (-default-http-listener-port, -default-https-listener-port),
	// which the default Gateway shares with Ingresses and VirtualServers.
	HTTPPort  int32
	HTTPSPort int32
	// DefaultGateway is the Gateway that shares the controller's address and
	// nginx's default ports with Ingresses (-default-gateway); every other
	// Gateway gets an address of its own.
	DefaultGateway types.NamespacedName
	// Namespace is the controller's namespace, where the Services of the
	// Gateways go.
	Namespace string
	// ExternalService is the controller's Service in Namespace: the Services
	// of the Gateways select the pods it selects.
	ExternalService string
	// ServiceType is the type of the Services of the Gateways.
	ServiceType v1.ServiceType
	// ListenerPortMin and ListenerPortMax are the range nginx's ports for
	// the listeners of Gateways come from (-gateway-listener-ports).
	ListenerPortMin, ListenerPortMax int32
	// ReservedPorts are ports nginx or the controller use otherwise.
	ReservedPorts []int32
	// IsLeader tells whether this replica is the leader, which writes the
	// Services of the Gateways.
	IsLeader func() bool
	// DynamicUpstreams tells that the wxingress module serves
	// (-dynamic-upstreams): two weighted backends are a canary of the
	// module, whose weights change without a reload.
	DynamicUpstreams bool
	// EnableSnippets tells that snippets are allowed (-enable-snippets): the
	// snippet annotations of routes are served, as an Ingress's.
	EnableSnippets bool
	// WatchNamespaces are the namespaces watched from the start, "" for
	// all; with -watch-namespace-label the controller adds and removes them.
	WatchNamespaces []string
	// Resync is the resync period of the informers.
	Resync time.Duration
}

// Controller turns Gateway API resources into VirtualServers through NGF's
// graph, and writes their status back.
type Controller struct {
	cfg    Config
	logger logr.Logger

	clusterInformers
	watches watches
	sources Sources

	validators validation.Validators

	// previous VirtualServers and TransportServers by key, to keep the
	// generation of unchanged ones
	previous   map[string]*conf_v1.VirtualServer
	previousTS map[string]*conf_v1.TransportServer
	// slicesWritten is what the leader last wrote to the EndpointSlices of
	// the Gateways' Services, by namespace and name
	slicesWritten map[string]string
	// handled is what the status was last written for, to clear the status
	// of what is gone since
	handled status.HandledStatusResources
}

// New makes a Controller. Its informers start with Start; it watches the
// namespaces of cfg.WatchNamespaces and those WatchNamespace adds.
func New(cfg Config) *Controller {
	if cfg.ControllerName == "" {
		cfg.ControllerName = DefaultControllerName
	}
	if cfg.GatewayClass == "" {
		cfg.GatewayClass = DefaultGatewayClass
	}
	if cfg.HTTPPort == 0 {
		cfg.HTTPPort = 80
	}
	if cfg.HTTPSPort == 0 {
		cfg.HTTPSPort = 443
	}

	c := &Controller{
		cfg:           cfg,
		logger:        logr.FromSlogHandler(cfg.Logger.Handler()).WithName("gateway"),
		watches:       watches{byNamespace: make(map[string]*nsWatch)},
		previous:      make(map[string]*conf_v1.VirtualServer),
		previousTS:    make(map[string]*conf_v1.TransportServer),
		slicesWritten: make(map[string]string),
		handled:       status.EmptyHandledStatusResources(),
		validators:    newValidators(),
	}
	c.newClusterInformers()
	for _, ns := range cfg.WatchNamespaces {
		c.WatchNamespace(ns)
	}
	return c
}

// SetSources gives the Controller the controller's listers of Services and
// Secrets.
func (c *Controller) SetSources(s Sources) {
	c.sources = s
}

// SetIsLeader tells the Controller how to know whether this replica is the
// leader, which writes the Services of the Gateways.
func (c *Controller) SetIsLeader(isLeader func() bool) {
	c.cfg.IsLeader = isLeader
}

// OnChange makes every change of a watched resource call fn. fn is called
// from the informers' goroutines and must only enqueue. Services and
// Secrets of the watched namespaces are the controller's: their changes are
// its to pass on.
func (c *Controller) OnChange(fn func()) error {
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	c.watches.onChange = fn
	if err := addHandler(c.clusterInformerList(), fn); err != nil {
		return err
	}
	for _, w := range c.watches.byNamespace {
		if err := addHandler(w.informers, fn); err != nil {
			return err
		}
	}
	return nil
}

// Start starts the informers.
func (c *Controller) Start(stop <-chan struct{}) {
	c.classFactory.Start(stop)
	c.nsFactory.Start(stop)
	c.ownFactory.Start(stop)
	c.svcFactory.Start(stop)
	c.watches.mu.Lock()
	defer c.watches.mu.Unlock()
	c.watches.started = stop
	for _, w := range c.watches.byNamespace {
		c.startWatch(w)
	}
}

// HasSynced lists what has to sync before the first Build: the cluster's
// informers and those of the namespaces watched from the start.
func (c *Controller) HasSynced() []cache.InformerSynced {
	var synced []cache.InformerSynced
	for _, inf := range c.clusterInformerList() {
		synced = append(synced, inf.HasSynced)
	}
	for _, w := range c.sortedWatches() {
		for _, inf := range w.informers {
			synced = append(synced, inf.HasSynced)
		}
	}
	return synced
}

// Result is one Build: the graph and the VirtualServers made from it.
type Result struct {
	Graph          *graph.Graph
	VirtualServers []*conf_v1.VirtualServer
	// TransportServers serve the TLS passthrough, TCP and UDP listeners.
	TransportServers []*conf_v1.TransportServer
	// Listeners are the nginx listeners of the Gateways with addresses of
	// their own, which the VirtualServers name.
	Listeners []conf_v1.Listener
	// Addresses are the addresses of the Gateways with addresses of their
	// own; the default Gateway reports the controller's.
	Addresses map[types.NamespacedName][]gatewayv1.GatewayStatusAddress
	// Skipped are the reasons parts of the graph are not served.
	Skipped []string
}

// Build evaluates the Gateway API resources and maps every Gateway of the
// class onto VirtualServers.
func (c *Controller) Build(ctx context.Context) (Result, error) {
	state, err := c.clusterState()
	if err != nil {
		return Result{}, err
	}
	return c.build(ctx, state), nil
}

func (c *Controller) build(ctx context.Context, state graph.ClusterState) Result {
	g := graph.BuildGraph(
		ctx,
		state,
		c.cfg.ControllerName,
		c.cfg.GatewayClass,
		"cluster.local",
		nil,          // NGINX Plus secrets
		noFetching{}, // WAF bundles: never fetched
		nil,          // PLM fetcher
		nil,
		nil,
		c.validators,
		c.logger,
		graph.FeatureFlags{},
		nil,
	)

	res := Result{Graph: g, Addresses: make(map[types.NamespacedName][]gatewayv1.GatewayStatusAddress)}
	addr, err := c.newAddressing()
	if err != nil {
		c.logger.Error(err, "listing the Services of the Gateways")
		addr = &addressing{c: c, used: map[int32]bool{}, existing: map[types.NamespacedName]*v1.Service{}, wanted: map[types.NamespacedName]bool{}}
	}

	byKey := make(map[string]*conf_v1.VirtualServer)
	tsByKey := make(map[string]*conf_v1.TransportServer)
	idx := newUpstreamIndex(g)
	for _, gw := range sortedGateways(g) {
		if !gw.Valid {
			continue
		}
		name := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}
		a := addr.address(ctx, gw)
		if !a.shared {
			res.Addresses[name] = a.addresses
		}
		if a.pending != "" {
			gw.Conditions = append(gw.Conditions, conditions.NewGatewayAddressNotAssigned(a.pending))
			res.Skipped = append(res.Skipped, fmt.Sprintf("Gateway %s: %s", name, a.pending))
		}
		dp := dataplane.BuildConfiguration(ctx, c.logger, g, gw, noEndpoints{}, false, ngfAPIv1alpha2.Dual)
		m := toVirtualServers(gw, dp, idx, mapOptions{
			dynamic:      c.cfg.DynamicUpstreams,
			snippets:     c.cfg.EnableSnippets,
			ingressClass: c.cfg.IngressClass,
			httpPort:     c.cfg.HTTPPort,
			httpsPort:    c.cfg.HTTPSPort,
		}, a)
		res.Skipped = append(res.Skipped, m.skipped...)
		res.Listeners = append(res.Listeners, m.listeners...)
		for _, vs := range m.virtualServers {
			key := vs.Namespace + "/" + vs.Name
			if _, dup := byKey[key]; dup {
				continue
			}
			byKey[key] = c.keepGeneration(key, vs)
			res.VirtualServers = append(res.VirtualServers, byKey[key])
		}
		l4 := toTransportServers(gw, dp, idx, mapOptions{ingressClass: c.cfg.IngressClass}, a)
		res.Skipped = append(res.Skipped, l4.skipped...)
		res.Listeners = append(res.Listeners, l4.listeners...)
		for _, ts := range l4.transportServers {
			key := ts.Namespace + "/" + ts.Name
			if _, dup := tsByKey[key]; dup {
				continue
			}
			tsByKey[key] = c.keepTSGeneration(key, ts)
			res.TransportServers = append(res.TransportServers, tsByKey[key])
		}
	}
	addr.collect(ctx)
	c.previous = byKey
	c.previousTS = tsByKey
	return res
}

// keepGeneration gives vs the generation of the VirtualServer with the same
// key from the last Build, or the next one if its spec changed: the
// controller regenerates a VirtualServer's config only when its generation
// changes.
func (c *Controller) keepGeneration(key string, vs *conf_v1.VirtualServer) *conf_v1.VirtualServer {
	prev, ok := c.previous[key]
	if !ok {
		vs.Generation = 1
		return vs
	}
	if equalVirtualServers(prev, vs) {
		return prev
	}
	vs.Generation = prev.Generation + 1
	return vs
}

// keepTSGeneration is keepGeneration for TransportServers.
func (c *Controller) keepTSGeneration(key string, ts *conf_v1.TransportServer) *conf_v1.TransportServer {
	prev, ok := c.previousTS[key]
	if !ok {
		ts.Generation = 1
		return ts
	}
	if prev.Namespace == ts.Namespace && prev.Name == ts.Name &&
		prev.CreationTimestamp.Equal(&ts.CreationTimestamp) &&
		reflect.DeepEqual(prev.Annotations, ts.Annotations) &&
		reflect.DeepEqual(prev.Spec, ts.Spec) {
		return prev
	}
	ts.Generation = prev.Generation + 1
	return ts
}

func (c *Controller) clusterState() (graph.ClusterState, error) {
	everything := labels.Everything()
	state := graph.ClusterState{
		GatewayClasses:  make(map[types.NamespacedName]*gatewayv1.GatewayClass),
		Gateways:        make(map[types.NamespacedName]*gatewayv1.Gateway),
		HTTPRoutes:      make(map[types.NamespacedName]*gatewayv1.HTTPRoute),
		GRPCRoutes:      make(map[types.NamespacedName]*gatewayv1.GRPCRoute),
		TLSRoutes:       make(map[types.NamespacedName]*gatewayv1.TLSRoute),
		TCPRoutes:       make(map[types.NamespacedName]*gatewayv1.TCPRoute),
		UDPRoutes:       make(map[types.NamespacedName]*gatewayv1.UDPRoute),
		Services:        make(map[types.NamespacedName]*v1.Service),
		Namespaces:      make(map[types.NamespacedName]*v1.Namespace),
		ReferenceGrants: make(map[types.NamespacedName]*gatewayv1.ReferenceGrant),
		Secrets:         make(map[types.NamespacedName]*v1.Secret),
	}

	gcs, err := c.gatewayClasses.List(everything)
	if err != nil {
		return state, err
	}
	for _, o := range gcs {
		state.GatewayClasses[types.NamespacedName{Name: o.Name}] = o
	}
	for _, w := range c.sortedWatches() {
		gws, err := w.gateways.List(everything)
		if err != nil {
			return state, err
		}
		for _, o := range gws {
			state.Gateways[types.NamespacedName{Namespace: o.Namespace, Name: o.Name}] = o
		}
		routes, err := w.httpRoutes.List(everything)
		if err != nil {
			return state, err
		}
		for _, o := range routes {
			state.HTTPRoutes[types.NamespacedName{Namespace: o.Namespace, Name: o.Name}] = o
		}
		if err := listInto(w.grpcRoutes.List, state.GRPCRoutes); err != nil {
			return state, err
		}
		if err := listInto(w.tlsRoutes.List, state.TLSRoutes); err != nil {
			return state, err
		}
		if err := listInto(w.tcpRoutes.List, state.TCPRoutes); err != nil {
			return state, err
		}
		if err := listInto(w.udpRoutes.List, state.UDPRoutes); err != nil {
			return state, err
		}
		grants, err := w.referenceGrants.List(everything)
		if err != nil {
			return state, err
		}
		for _, o := range grants {
			state.ReferenceGrants[types.NamespacedName{Namespace: o.Namespace, Name: o.Name}] = o
		}
	}
	if c.sources != nil {
		for _, o := range c.sources.ListServices() {
			state.Services[types.NamespacedName{Namespace: o.Namespace, Name: o.Name}] = o
		}
		for _, o := range c.sources.ListSecrets() {
			state.Secrets[types.NamespacedName{Namespace: o.Namespace, Name: o.Name}] = o
		}
	}
	nss, err := c.namespaces.List(everything)
	if err != nil {
		return state, err
	}
	for _, o := range nss {
		state.Namespaces[types.NamespacedName{Name: o.Name}] = o
	}
	return state, nil
}

// listInto adds what list lists to into, by namespace and name.
func listInto[T interface {
	GetNamespace() string
	GetName() string
}](list func(labels.Selector) ([]T, error), into map[types.NamespacedName]T) error {
	objs, err := list(labels.Everything())
	if err != nil {
		return err
	}
	for _, o := range objs {
		into[types.NamespacedName{Namespace: o.GetNamespace(), Name: o.GetName()}] = o
	}
	return nil
}

// newValidators are the checks the graph applies to route fields: NGF's for
// nginx, which is wxingress's data plane as well.
func newValidators() validation.Validators {
	return validation.Validators{
		HTTPFieldsValidator: ngxvalidation.HTTPValidator{},
		GenericValidator:    ngxvalidation.GenericValidator{},
		AuthFieldsValidator: ngxvalidation.AuthFieldValidator{},
		PolicyValidator:     noPolicies{},
	}
}

// noPolicies validates NGF's own policy CRDs, which wxingress does not watch:
// there are none to validate.
type noPolicies struct{}

func (noPolicies) Validate(policies.Policy) []conditions.Condition { return nil }

func (noPolicies) ValidateGlobalSettings(policies.Policy, *policies.GlobalSettings) []conditions.Condition {
	return nil
}
func (noPolicies) Conflicts(policies.Policy, policies.Policy) bool { return false }

// errNoFetching is what noFetching answers.
var errNoFetching = errors.New("wxingress does not fetch WAF bundles")

// noFetching is the WAF bundle fetcher NGF's graph is given: wxingress
// contacts no external endpoint, so it refuses every fetch.
type noFetching struct{}

func (noFetching) FetchPolicyBundle(context.Context, fetch.Request) (fetch.Result, error) {
	return fetch.Result{}, errNoFetching
}

func (noFetching) FetchLogProfileBundle(context.Context, fetch.Request) (fetch.Result, error) {
	return fetch.Result{}, errNoFetching
}

func (noFetching) FetchPolicyBundleChecksum(context.Context, fetch.Request) (string, error) {
	return "", errNoFetching
}

func (noFetching) FetchLogProfileBundleChecksum(context.Context, fetch.Request) (string, error) {
	return "", errNoFetching
}

// noEndpoints is the service resolver of the dataplane conversion. The
// VirtualServers name Services, whose endpoints the controller resolves
// itself, as for any VirtualServer - and with -dynamic-upstreams changes
// without a reload.
type noEndpoints struct{}

func (noEndpoints) Resolve(
	context.Context,
	logr.Logger,
	types.NamespacedName,
	v1.ServicePort,
	[]discoveryV1.AddressType,
) ([]resolver.Endpoint, error) {
	return nil, nil
}
