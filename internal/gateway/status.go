package gateway

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/status"
)

// WriteStatus writes the status NGF's status package builds from the graph
// of a Build: the GatewayClass, every Gateway of the class with its
// addresses - those of its Service, or sharedAddresses, the controller's,
// for the default Gateway -, its listeners and whether its config is
// programmed (errs has the errors of generating and loading it, by Gateway),
// every route, and the status of routes no longer handled is removed. Only
// the leader calls it.
func (c *Controller) WriteStatus(
	ctx context.Context,
	res Result,
	sharedAddresses []gatewayv1.GatewayStatusAddress,
	errs map[types.NamespacedName]error,
) {
	g := res.Graph
	if g == nil {
		return
	}
	now := metav1.Now()
	current, dropped := status.HandledStatusResourcesFromGraph(c.handled, g)
	c.handled = current

	reqs := status.PrepareGatewayClassRequests(g.GatewayClass, g.IgnoredGatewayClasses, now)
	reqs = append(reqs, status.PrepareActiveRouteRequests(g.L4Routes, g.Routes, now, c.cfg.ControllerName)...)
	reqs = append(reqs, status.PrepareDroppedRequests(dropped, c.cfg.ControllerName)...)
	for _, gw := range sortedGateways(g) {
		name := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}
		addresses := sharedAddresses
		if own, ok := res.Addresses[name]; ok {
			addresses = own
		}
		reqs = append(reqs, status.PrepareGatewayRequests(gw, now, addresses, graph.NginxReloadResult{Error: errs[name]})...)
	}

	for _, r := range reqs {
		if err := c.write(ctx, r); err != nil {
			c.logger.Error(err, "writing Gateway API status", "kind", fmt.Sprintf("%T", r.ResourceType), "name", r.NsName.String())
		}
	}
}

// write applies one status request: to the object from the informer and,
// if that was stale, once more to the one from the API server. It writes
// only a status that changed - every write comes back as an event. A
// GatewayClass gets wxingress's supported features, not NGF's.
func (c *Controller) write(ctx context.Context, r status.UpdateRequest) error {
	gw := c.cfg.GatewayClient.GatewayV1()
	ns, name := r.NsName.Namespace, r.NsName.Name

	var err error
	for attempt := 0; attempt < 2; attempt++ {
		fresh := attempt > 0
		switch r.ResourceType.(type) {
		case *gatewayv1.GatewayClass:
			setter := func(o client.Object) bool {
				gc := o.(*gatewayv1.GatewayClass)
				r.Setter(gc)
				if len(gc.Status.SupportedFeatures) > 0 {
					gc.Status.SupportedFeatures = supportedFeatures()
				}
				return true
			}
			err = writeObject(ctx, fresh, name, setter,
				func() (*gatewayv1.GatewayClass, error) { return c.gatewayClasses.Get(name) },
				gw.GatewayClasses().Get, gw.GatewayClasses().UpdateStatus,
				func(o *gatewayv1.GatewayClass) any { return o.Status })
		case *gatewayv1.Gateway:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.Gateway, error) { return c.cached(ns, name, (*nsWatch).gateway) },
				gw.Gateways(ns).Get, gw.Gateways(ns).UpdateStatus,
				func(o *gatewayv1.Gateway) any { return o.Status })
		case *gatewayv1.HTTPRoute:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.HTTPRoute, error) { return cachedIn(c, ns, name, (*nsWatch).httpRoute) },
				gw.HTTPRoutes(ns).Get, gw.HTTPRoutes(ns).UpdateStatus,
				func(o *gatewayv1.HTTPRoute) any { return o.Status })
		case *gatewayv1.GRPCRoute:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.GRPCRoute, error) { return cachedIn(c, ns, name, (*nsWatch).grpcRoute) },
				gw.GRPCRoutes(ns).Get, gw.GRPCRoutes(ns).UpdateStatus,
				func(o *gatewayv1.GRPCRoute) any { return o.Status })
		case *gatewayv1.TLSRoute:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.TLSRoute, error) { return cachedIn(c, ns, name, (*nsWatch).tlsRoute) },
				gw.TLSRoutes(ns).Get, gw.TLSRoutes(ns).UpdateStatus,
				func(o *gatewayv1.TLSRoute) any { return o.Status })
		case *gatewayv1.TCPRoute:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.TCPRoute, error) { return cachedIn(c, ns, name, (*nsWatch).tcpRoute) },
				gw.TCPRoutes(ns).Get, gw.TCPRoutes(ns).UpdateStatus,
				func(o *gatewayv1.TCPRoute) any { return o.Status })
		case *gatewayv1.UDPRoute:
			err = writeObject(ctx, fresh, name, r.Setter,
				func() (*gatewayv1.UDPRoute, error) { return cachedIn(c, ns, name, (*nsWatch).udpRoute) },
				gw.UDPRoutes(ns).Get, gw.UDPRoutes(ns).UpdateStatus,
				func(o *gatewayv1.UDPRoute) any { return o.Status })
		default:
			// kinds wxingress does not serve have no status to write
			return nil
		}
		if err == nil || apierrors.IsNotFound(err) {
			return nil
		}
		if !apierrors.IsConflict(err) {
			return err
		}
	}
	return err
}

// writeObject writes the status setter sets on an object - from the
// informer, or fresh from the API server - when it differs from what it has.
func writeObject[T client.Object](
	ctx context.Context,
	fresh bool,
	name string,
	setter status.Setter,
	cached func() (T, error),
	get func(context.Context, string, metav1.GetOptions) (T, error),
	update func(context.Context, T, metav1.UpdateOptions) (T, error),
	statusOf func(T) any,
) error {
	var obj T
	var err error
	if fresh {
		obj, err = get(ctx, name, metav1.GetOptions{})
	} else {
		obj, err = cached()
	}
	if err != nil {
		return err
	}
	before := statusOf(obj)
	cp, ok := obj.DeepCopyObject().(T)
	if !ok {
		return fmt.Errorf("a copy of %s is no %T", name, obj)
	}
	if !setter(cp) || equality.Semantic.DeepEqual(before, statusOf(cp)) {
		return nil
	}
	_, err = update(ctx, cp, metav1.UpdateOptions{})
	return err
}

// cached is a Gateway in the informers; one of a namespace no longer
// watched is not found.
func (c *Controller) cached(ns, name string, get func(*nsWatch, string, string) (*gatewayv1.Gateway, error)) (*gatewayv1.Gateway, error) {
	return cachedIn(c, ns, name, get)
}

// cachedIn is an object of a watched namespace in the informers.
func cachedIn[T any](c *Controller, ns, name string, get func(*nsWatch, string, string) (T, error)) (T, error) {
	w := c.watchOf(ns)
	if w == nil {
		var none T
		return none, apierrors.NewNotFound(gatewayv1.Resource("routes"), name)
	}
	return get(w, ns, name)
}

// cachedGateway is a Gateway in the informers.
func (c *Controller) cachedGateway(ns, name string) (*gatewayv1.Gateway, error) {
	return cachedIn(c, ns, name, (*nsWatch).gateway)
}

func (w *nsWatch) gateway(ns, name string) (*gatewayv1.Gateway, error) {
	return w.gateways.Gateways(ns).Get(name)
}

func (w *nsWatch) httpRoute(ns, name string) (*gatewayv1.HTTPRoute, error) {
	return w.httpRoutes.HTTPRoutes(ns).Get(name)
}

func (w *nsWatch) grpcRoute(ns, name string) (*gatewayv1.GRPCRoute, error) {
	return w.grpcRoutes.GRPCRoutes(ns).Get(name)
}

func (w *nsWatch) tlsRoute(ns, name string) (*gatewayv1.TLSRoute, error) {
	return w.tlsRoutes.TLSRoutes(ns).Get(name)
}

func (w *nsWatch) tcpRoute(ns, name string) (*gatewayv1.TCPRoute, error) {
	return w.tcpRoutes.TCPRoutes(ns).Get(name)
}

func (w *nsWatch) udpRoute(ns, name string) (*gatewayv1.UDPRoute, error) {
	return w.udpRoutes.UDPRoutes(ns).Get(name)
}
