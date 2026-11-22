package k8s

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	api_v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/gateway"
	nl "github.com/wx-one/wxingress/internal/logger"
	conf_v1 "github.com/wx-one/wxingress/pkg/apis/configuration/v1"
)

// gatewayAPIKey is the key of the one task that evaluates all Gateway API
// resources: the workqueue holds it once however many changes ask for it.
const gatewayAPIKey = "gateway-api"

// The Gateway API (-enable-gateway-api) goes through NGF's graph in
// internal/gateway, which maps every Gateway of the class onto
// VirtualServers. They take the path of VirtualServers from the API - host
// conflicts with Ingresses and VirtualServers, the version2 templates and,
// with -dynamic-upstreams, endpoints without reloads - but have no object
// in the API: their status goes to the Gateway API resources instead.

func (lbc *LoadBalancerController) enqueueGatewayAPI() {
	if lbc.gatewayAPI != nil {
		lbc.syncQueue.Add(task{Kind: gatewayAPI, Key: gatewayAPIKey})
	}
}

// syncGatewayAPI evaluates the Gateway API resources, replaces the
// VirtualServers made from them, and writes their status.
func (lbc *LoadBalancerController) syncGatewayAPI() {
	ctx := lbc.runContext()
	res, err := lbc.gatewayAPI.Build(ctx)
	if err != nil {
		nl.Errorf(lbc.Logger, "Error evaluating the Gateway API resources: %v", err)
		return
	}
	for _, s := range res.Skipped {
		nl.Debugf(lbc.Logger, "Gateway API: %s", s)
	}
	for _, w := range res.Warnings {
		nl.Warnf(lbc.Logger, "Gateway API: %s", w)
	}

	if lbc.gatewayErrors == nil {
		lbc.gatewayErrors = make(map[string]gatewayError)
	}
	current := make(map[string]bool, len(res.VirtualServers)+len(res.TransportServers))
	for _, vs := range res.VirtualServers {
		current[getResourceKey(&vs.ObjectMeta)] = true
	}
	for _, ts := range res.TransportServers {
		current[getResourceKey(&ts.ObjectMeta)] = true
	}
	for key := range lbc.gatewayErrors {
		if !current[key] {
			delete(lbc.gatewayErrors, key)
		}
	}

	// the default server of every port of a Gateway answers what none of its
	// hostnames is for
	if err := lbc.configurator.UpdateGatewayDefaultServer(gatewayDefaultServerPorts(res.Listeners)); err != nil {
		nl.Errorf(lbc.Logger, "Error updating the default server of the Gateways' ports: %v", err)
	}
	changes, problems := lbc.configuration.ReplaceGatewayVirtualServers(res.VirtualServers, res.Listeners)
	tsChanges, tsProblems := lbc.configuration.ReplaceGatewayTransportServers(res.TransportServers)
	changes = append(changes, tsChanges...)
	problems = append(problems, tsProblems...)
	for _, p := range problems {
		switch o := p.Object.(type) {
		case *conf_v1.VirtualServer:
			if gateway.IsGatewayVirtualServer(o) {
				nl.Warnf(lbc.Logger, "Gateway API: Gateway %s: %s", o.Annotations[gateway.AnnotationGateway], p.Message)
				lbc.gatewayErrors[getResourceKey(&o.ObjectMeta)] = gatewayError{gateway: gatewayOfAnnotations(o.Annotations), err: gatewayProblem(o.Annotations, p.Message)}
			}
		case *conf_v1.TransportServer:
			if gateway.IsGatewayTransportServer(o) {
				nl.Warnf(lbc.Logger, "Gateway API: Gateway %s: %s", o.Annotations[gateway.AnnotationGateway], p.Message)
				lbc.gatewayErrors[getResourceKey(&o.ObjectMeta)] = gatewayError{gateway: gatewayOfAnnotations(o.Annotations), err: gatewayProblem(o.Annotations, p.Message)}
			}
		}
	}
	lbc.processChanges(changes)
	lbc.processProblems(problems)

	if !lbc.isNginxReady || !lbc.reportCustomResourceStatusEnabled() {
		return
	}
	// a route is Accepted when nginx serves it: with a reload that waits
	// (-dynamic-upstreams rate-limits them), the status waits for it too
	if lbc.configurator.ReloadAsked() {
		lbc.gatewayStatusAfterReload = true
		return
	}
	lbc.gatewayStatusAfterReload = false
	errs := make(map[types.NamespacedName]error)
	for _, e := range lbc.gatewayErrors {
		errs[e.gateway] = errors.Join(errs[e.gateway], e.err)
	}
	lbc.gatewayAPI.WriteStatus(ctx, res, lbc.gatewayAddresses(), errs)
}

// gatewayDefaultServerPorts are the nginx ports of the Gateways' listeners,
// for HTTP and for HTTPS.
func gatewayDefaultServerPorts(listeners []conf_v1.Listener) (httpPorts, httpsPorts []int) {
	for _, l := range listeners {
		if l.Protocol != conf_v1.HTTPProtocol {
			continue
		}
		if l.Ssl {
			httpsPorts = append(httpsPorts, l.Port)
		} else {
			httpPorts = append(httpPorts, l.Port)
		}
	}
	slices.Sort(httpPorts)
	slices.Sort(httpsPorts)
	return slices.Compact(httpPorts), slices.Compact(httpsPorts)
}

// virtualServerHostKey is the key of the host of a VirtualServer among the
// hosts of the configuration: its host, which an Ingress or another
// VirtualServer can take - unless it is made from a Gateway with an address
// of its own, whose listeners no other resource is on.
func virtualServerHostKey(vs *conf_v1.VirtualServer) string {
	if vs.Spec.Listener == nil || !gateway.IsGatewayVirtualServer(vs) {
		return vs.Spec.Host
	}
	return vs.Spec.Host + " " + vs.Spec.Listener.HTTP + " " + vs.Spec.Listener.HTTPS
}

// gatewayStatusAfterReloaded enqueues the Gateway API once more after the
// reload its status waited for.
func (lbc *LoadBalancerController) gatewayStatusAfterReloaded() {
	if lbc.gatewayStatusAfterReload && !lbc.configurator.ReloadAsked() {
		lbc.gatewayStatusAfterReload = false
		lbc.enqueueGatewayAPI()
	}
}

// gatewaySources are the controller's Services and Secrets, which the
// Gateway API reads: those of the watched namespaces, as -watch-namespace,
// -watch-namespace-label and -watch-secret-namespace have it.
type gatewaySources struct {
	lbc *LoadBalancerController
}

func (s gatewaySources) ListServices() []*api_v1.Service {
	var out []*api_v1.Service
	s.lbc.namespacedInformers.ForEach(func(nsi *namespacedInformer) {
		if nsi.svcLister == nil {
			return
		}
		for _, o := range nsi.svcLister.List() {
			if svc, ok := o.(*api_v1.Service); ok {
				out = append(out, svc)
			}
		}
	})
	return out
}

func (s gatewaySources) ListSecrets() []*api_v1.Secret {
	var out []*api_v1.Secret
	s.lbc.namespacedInformers.ForEach(func(nsi *namespacedInformer) {
		if !nsi.isSecretsEnabledNamespace || nsi.secretLister == nil {
			return
		}
		for _, o := range nsi.secretLister.List() {
			if secret, ok := o.(*api_v1.Secret); ok {
				out = append(out, secret)
			}
		}
	})
	return out
}

// gatewayError is why the config of a VirtualServer made from a Gateway is
// not in nginx.
type gatewayError struct {
	gateway types.NamespacedName
	err     error
}

// gatewayVirtualServerApplied takes the place of the status and events of a
// VirtualServer made from a Gateway: an error goes to the Gateway's
// Programmed condition.
func (lbc *LoadBalancerController) gatewayVirtualServerApplied(vs *conf_v1.VirtualServer, err error) {
	key := getResourceKey(&vs.ObjectMeta)
	if err == nil {
		delete(lbc.gatewayErrors, key)
		return
	}
	nl.Errorf(lbc.Logger, "Error applying the config of Gateway %s for %s: %v",
		vs.Annotations[gateway.AnnotationGateway], vs.Spec.Host, err)
	if lbc.gatewayErrors == nil {
		lbc.gatewayErrors = make(map[string]gatewayError)
	}
	lbc.gatewayErrors[key] = gatewayError{gateway: gatewayOf(vs), err: gatewayApplyError(vs.Annotations)}
}

func gatewayOf(vs *conf_v1.VirtualServer) types.NamespacedName {
	return gatewayOfAnnotations(vs.Annotations)
}

func gatewayOfAnnotations(annotations map[string]string) types.NamespacedName {
	ns, name, _ := strings.Cut(annotations[gateway.AnnotationGateway], "/")
	return types.NamespacedName{Namespace: ns, Name: name}
}

// gatewayTransportServerApplied is gatewayVirtualServerApplied for a
// TransportServer made from a Gateway.
func (lbc *LoadBalancerController) gatewayTransportServerApplied(ts *conf_v1.TransportServer, err error) {
	key := getResourceKey(&ts.ObjectMeta)
	if err == nil {
		delete(lbc.gatewayErrors, key)
		return
	}
	nl.Errorf(lbc.Logger, "Error applying the config of Gateway %s for port %s: %v",
		ts.Annotations[gateway.AnnotationGateway], ts.Annotations[gateway.AnnotationPort], err)
	if lbc.gatewayErrors == nil {
		lbc.gatewayErrors = make(map[string]gatewayError)
	}
	lbc.gatewayErrors[key] = gatewayError{gateway: gatewayOfAnnotations(ts.Annotations), err: gatewayApplyError(ts.Annotations)}
}

// The status of a Gateway is read by those who may read the Gateway, and a
// Gateway takes routes of other namespaces: the errors of its config there
// say only which of its hostnames and ports they are about, never the
// message of the validator, of the host conflict or of nginx - which can
// name the hosts, resources and config of others. The log has the message.

// gatewayServed says which part of a Gateway the config made from it with
// annotations serves: its hostname and listener port.
func gatewayServed(annotations map[string]string) string {
	host := annotations[gateway.AnnotationHostname]
	what := "listeners without a hostname"
	if host != "" && !gateway.IsCatchAllHost(host) {
		what = fmt.Sprintf("hostname %q", host)
	}
	if port := annotations[gateway.AnnotationPort]; port != "" {
		what += " on listener port " + port
	}
	return what
}

// gatewayProblem is the error a Gateway's status gets for a problem of the
// config made from it.
func gatewayProblem(annotations map[string]string, message string) error {
	if strings.HasSuffix(message, "is taken by another resource") {
		return fmt.Errorf("%s: not served, the host is taken by another resource", gatewayServed(annotations))
	}
	return fmt.Errorf("%s: not served, its configuration was rejected - the controller's log has the reason", gatewayServed(annotations))
}

// gatewayApplyError is the error a Gateway's status gets when the config
// made from it could not be applied.
func gatewayApplyError(annotations map[string]string) error {
	return fmt.Errorf("%s: not served, its configuration could not be applied - the controller's log has the reason", gatewayServed(annotations))
}

// gatewayAddresses are the addresses the Gateways report: those the
// controller reports for Ingresses, else the cluster IPs of its Service.
func (lbc *LoadBalancerController) gatewayAddresses() []gatewayv1.GatewayStatusAddress {
	var out []gatewayv1.GatewayStatusAddress
	ipType, hostType := gatewayv1.IPAddressType, gatewayv1.HostnameAddressType
	su := lbc.statusUpdater
	for _, s := range su.status {
		if s.IP != "" {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &ipType, Value: s.IP})
		}
		if s.Hostname != "" {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &hostType, Value: s.Hostname})
		}
	}
	if len(out) > 0 || su.externalServiceName == "" {
		return out
	}
	svc, err := lbc.getServiceFromInformer(su.namespace, su.externalServiceName)
	if err != nil {
		nl.Debugf(lbc.Logger, "Gateway API: no address from Service %s/%s: %v", su.namespace, su.externalServiceName, err)
		return out
	}
	for _, ip := range svc.Spec.ClusterIPs {
		if ip != "" && ip != api_v1.ClusterIPNone {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &ipType, Value: ip})
		}
	}
	return out
}

// ReplaceGatewayVirtualServers makes vses the VirtualServers made from
// Gateways, and listeners the listeners they name, in place of those of the
// last call. A VirtualServer whose generation did not change is left as it
// is, so that an unchanged Gateway regenerates nothing.
func (c *Configuration) ReplaceGatewayVirtualServers(vses []*conf_v1.VirtualServer, listeners []conf_v1.Listener) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	keep := make(map[string]bool, len(vses))
	changed := false
	var problems []ConfigurationProblem

	if !slices.Equal(c.gatewayListeners, listeners) {
		c.gatewayListeners = slices.Clone(listeners)
		c.setGlobalConfigListenerMap()
		c.gatewayListenersChanged = true
		changed = true
	}

	for _, vs := range vses {
		key := getResourceKey(&vs.ObjectMeta)
		keep[key] = true
		prev, exists := c.virtualServers[key]
		if exists && prev.Generation == vs.Generation {
			continue
		}
		if err := c.virtualServerValidator.ValidateVirtualServer(gateway.ForValidation(vs)); err != nil {
			if exists {
				delete(c.virtualServers, key)
				changed = true
			}
			problems = append(problems, ConfigurationProblem{
				Object:  vs,
				IsError: true,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("the VirtualServer of Gateway %s for %s was rejected: %v", vs.Annotations[gateway.AnnotationGateway], vs.Spec.Host, err),
			})
			continue
		}
		// balanceUpstreamProxies changes what it is given; the Gateway layer
		// keeps its own copy to compare the next build with
		stored := vs.DeepCopy()
		c.balanceUpstreamProxies(stored.Spec.Upstreams)
		c.virtualServers[key] = stored
		changed = true
	}
	for key, vs := range c.virtualServers {
		if gateway.IsGatewayVirtualServer(vs) && !keep[key] {
			delete(c.virtualServers, key)
			changed = true
		}
	}

	if !changed || !c.startupComplete {
		return nil, problems
	}
	changes, hostProblems := c.rebuildHosts()
	return changes, append(problems, hostProblems...)
}

// ReplaceGatewayTransportServers makes tses the TransportServers made from
// Gateways, in place of those of the last call, as
// ReplaceGatewayVirtualServers does for VirtualServers; it follows that
// call, which sets the listeners they name.
func (c *Configuration) ReplaceGatewayTransportServers(tses []*conf_v1.TransportServer) ([]ResourceChange, []ConfigurationProblem) {
	c.lock.Lock()
	defer c.lock.Unlock()

	keep := make(map[string]bool, len(tses))
	changed := c.gatewayListenersChanged
	c.gatewayListenersChanged = false
	var problems []ConfigurationProblem

	for _, ts := range tses {
		key := getResourceKey(&ts.ObjectMeta)
		keep[key] = true
		prev, exists := c.transportServers[key]
		if exists && prev.Generation == ts.Generation {
			continue
		}
		if err := c.transportServerValidator.ValidateTransportServer(gateway.TransportServerForValidation(ts)); err != nil {
			if exists {
				delete(c.transportServers, key)
				changed = true
			}
			problems = append(problems, ConfigurationProblem{
				Object:  ts,
				IsError: true,
				Reason:  nl.EventReasonRejected,
				Message: fmt.Sprintf("the TransportServer of Gateway %s for port %s was rejected: %v",
					ts.Annotations[gateway.AnnotationGateway], ts.Annotations[gateway.AnnotationPort], err),
			})
			continue
		}
		c.transportServers[key] = ts
		changed = true
	}
	for key, ts := range c.transportServers {
		if gateway.IsGatewayTransportServer(ts) && !keep[key] {
			delete(c.transportServers, key)
			changed = true
		}
	}

	if !changed {
		return nil, problems
	}
	changes, listenerProblems := c.rebuildListenerHosts()
	return changes, append(problems, listenerProblems...)
}

// gatewayEventFilter drops the events of VirtualServers made from Gateways,
// which have no object in the API to be about.
type gatewayEventFilter struct {
	record.EventRecorder
}

func isGatewayObject(obj runtime.Object) bool {
	switch o := obj.(type) {
	case *conf_v1.VirtualServer:
		return gateway.IsGatewayVirtualServer(o)
	case *conf_v1.TransportServer:
		return gateway.IsGatewayTransportServer(o)
	}
	return false
}

func (f gatewayEventFilter) Event(obj runtime.Object, eventtype, reason, message string) {
	if !isGatewayObject(obj) {
		f.EventRecorder.Event(obj, eventtype, reason, message)
	}
}

func (f gatewayEventFilter) Eventf(obj runtime.Object, eventtype, reason, messageFmt string, args ...interface{}) {
	if !isGatewayObject(obj) {
		f.EventRecorder.Eventf(obj, eventtype, reason, messageFmt, args...)
	}
}

func (f gatewayEventFilter) AnnotatedEventf(obj runtime.Object, annotations map[string]string, eventtype, reason, messageFmt string, args ...interface{}) {
	if !isGatewayObject(obj) {
		f.EventRecorder.AnnotatedEventf(obj, annotations, eventtype, reason, messageFmt, args...)
	}
}
