package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/conditions"
	"github.com/wx-one/wxingress/internal/third_party/ngf/controller/state/graph"
)

// Every Gateway but the default one has an address of its own: a Service in
// the Gateway's namespace, owned by the Gateway, that maps the ports of its
// listeners onto ports nginx listens on for this Gateway alone. One nginx
// serves all Gateways; the ports keep them apart, so that a request to the
// address of one Gateway never reaches the routes of another.
//
// The controller's pods are in another namespace, and a Service selects pods
// of its own namespace only: the Service has no selector, and the leader
// writes its EndpointSlice - the addresses of the controller's own Service
// (-external-service), with the nginx ports - and keeps it up to date as the
// controller's pods come and go.
//
// The Service is the record of which nginx port a listener port got: every
// replica reads it, only the leader writes it. A replica that finds no
// Service, or no port in it for a listener, does not serve that listener yet.
//
// The default Gateway (-default-gateway) shares the controller's address and
// nginx's default ports with Ingresses and VirtualServers - for a migration
// from ingress-nginx, where the hosts stay where they are. That is a
// deliberate deviation from the Gateway API, which wants every Gateway to have
// addresses of its own.

const (
	// LabelManagedBy marks the Services of Gateways.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// managedBy is the value of LabelManagedBy.
	managedBy = "wxingress-gateway"
	// LabelGatewayClass is the GatewayClass of the Gateway a Service is for.
	LabelGatewayClass = "wxingress.wizardtales.com/gateway-class"
	// LabelGatewayName and LabelGatewayNamespace name the Gateway.
	LabelGatewayName      = "gateway.networking.k8s.io/gateway-name"
	LabelGatewayNamespace = "wxingress.wizardtales.com/gateway-namespace"
)

// listenerPort is a port of a Gateway's listeners: TCP for HTTP, HTTPS, TLS
// and TCP listeners, UDP for UDP listeners.
type listenerPort struct {
	port     int32
	protocol v1.Protocol
}

func (p listenerPort) portName() string {
	return strings.ToLower(string(p.protocol)) + "-" + strconv.Itoa(int(p.port))
}

// gatewayAddress is where a Gateway is served.
type gatewayAddress struct {
	// shared tells that the Gateway is the default one, on nginx's default
	// ports and the controller's address.
	shared bool
	// nginxPorts maps the ports of the listeners onto the ports nginx listens
	// on for them. Only for a Gateway that is not the default one.
	nginxPorts map[listenerPort]int32
	// addresses are those of its Service; nil for the default Gateway, which
	// reports the controller's.
	addresses []gatewayv1.GatewayStatusAddress
	// pending is why the Gateway has no address yet, if it has none.
	pending string
}

func protocolOf(p gatewayv1.ProtocolType) (v1.Protocol, bool) {
	switch p {
	case gatewayv1.HTTPProtocolType, gatewayv1.HTTPSProtocolType, gatewayv1.TLSProtocolType, gatewayv1.TCPProtocolType:
		return v1.ProtocolTCP, true
	case gatewayv1.UDPProtocolType:
		return v1.ProtocolUDP, true
	}
	return "", false
}

// wantedPorts are the ports of the valid listeners of gw.
func wantedPorts(gw *graph.Gateway) []listenerPort {
	set := make(map[listenerPort]bool)
	for _, l := range gw.Listeners {
		if !l.Valid {
			continue
		}
		proto, ok := protocolOf(l.Source.Protocol)
		if !ok {
			continue
		}
		set[listenerPort{port: l.Source.Port, protocol: proto}] = true
	}
	return slices.SortedFunc(maps.Keys(set), func(a, b listenerPort) int {
		if a.port != b.port {
			return int(a.port - b.port)
		}
		return strings.Compare(string(a.protocol), string(b.protocol))
	})
}

// restrictDefaultGateway leaves the default Gateway only the listeners the
// controller's Service has: HTTP on 80 and HTTPS on 443. Every other one is
// not accepted, its port is unavailable.
func restrictDefaultGateway(gw *graph.Gateway) {
	for _, l := range gw.Listeners {
		if !l.Valid {
			continue
		}
		ok := (l.Source.Port == 80 && l.Source.Protocol == gatewayv1.HTTPProtocolType) ||
			(l.Source.Port == 443 && l.Source.Protocol == gatewayv1.HTTPSProtocolType)
		if ok {
			continue
		}
		l.Valid = false
		msg := fmt.Sprintf("the default Gateway shares the address of the controller, which serves HTTP on 80 and HTTPS on 443 only, not %s on %d",
			l.Source.Protocol, l.Source.Port)
		l.Conditions = append(l.Conditions,
			conditions.Condition{
				Type:    string(gatewayv1.ListenerConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonPortUnavailable),
				Message: msg,
			},
			conditions.NewListenerNotProgrammedInvalid(msg),
		)
	}
}

// restrictTLSListeners rejects the TLS listeners of a port that has both
// Passthrough and Terminate: nginx either reads the ClientHello of a
// connection or terminates it, per port - mixed modes are a protocol
// conflict (TLSRouteModeMixed is not supported).
func restrictTLSListeners(gw *graph.Gateway) {
	modes := make(map[int32]map[gatewayv1.TLSModeType]bool)
	for _, l := range gw.Listeners {
		if !l.Valid || l.Source.Protocol != gatewayv1.TLSProtocolType {
			continue
		}
		mode := gatewayv1.TLSModeTerminate
		if l.Source.TLS != nil && l.Source.TLS.Mode != nil {
			mode = *l.Source.TLS.Mode
		}
		if modes[l.Source.Port] == nil {
			modes[l.Source.Port] = make(map[gatewayv1.TLSModeType]bool)
		}
		modes[l.Source.Port][mode] = true
	}
	for _, l := range gw.Listeners {
		if !l.Valid || l.Source.Protocol != gatewayv1.TLSProtocolType || len(modes[l.Source.Port]) < 2 {
			continue
		}
		msg := fmt.Sprintf("port %d has TLS listeners with Passthrough and with Terminate, which nginx cannot serve on one port", l.Source.Port)
		l.Valid = false
		l.SupportedKinds = nil
		l.L4Routes = nil
		l.Conditions = append(l.Conditions,
			conditions.Condition{
				Type:    string(gatewayv1.ListenerConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonProtocolConflict),
				Message: msg,
			},
			conditions.NewListenerNotProgrammedInvalid(msg),
		)
	}
}

// restrictFrontendValidation rejects the listeners that ask for client
// certificates (spec.tls.frontend of the Gateway): the VirtualServers and
// TransportServers the listeners map onto would serve them without the
// verification, to any client. Such a listener is not accepted and not
// served - an ingressMTLS Policy (wxingress.wizardtales.com/policies) on
// the Gateway or its routes verifies client certificates instead.
func restrictFrontendValidation(gw *graph.Gateway) {
	for _, l := range gw.Listeners {
		if !l.Valid || (len(l.CACertificateRefs) == 0 && l.ValidationMode == "") {
			continue
		}
		msg := "client certificate validation (spec.tls.frontend) is not supported: the listener is not served; " +
			"use an ingressMTLS Policy with the annotation " + AnnotationPolicies
		l.Valid = false
		l.SupportedKinds = nil
		l.L4Routes = nil
		l.Conditions = append(l.Conditions,
			conditions.Condition{
				Type:    string(gatewayv1.ListenerConditionAccepted),
				Status:  metav1.ConditionFalse,
				Reason:  string(gatewayv1.ListenerReasonUnsupportedValue),
				Message: msg,
			},
			conditions.NewListenerNotProgrammedInvalid(msg),
		)
	}
}

// ServiceName is the name of the Service of a Gateway in its namespace: its
// name and the class, as NGF names it, if that is a valid Service name;
// otherwise a shortened one with a hash.
func ServiceName(gw types.NamespacedName, class string) string {
	name := gw.Name + "-" + class
	if len(validation.IsDNS1035Label(name)) == 0 {
		return name
	}
	sum := sha256.Sum256([]byte(gw.String() + "\n" + class))
	suffix := "-" + hex.EncodeToString(sum[:5])
	base := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		}
		return '-'
	}, strings.ToLower(gw.Name))
	base = strings.Trim(base, "-")
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = "gw-" + base
	}
	if max := validation.DNS1035LabelMaxLength - len(suffix); len(base) > max {
		base = strings.TrimRight(base[:max], "-")
	}
	return base + suffix
}

// serviceAddresses are the addresses of a Gateway's Service: those of its
// load balancer, else its cluster IPs.
func serviceAddresses(svc *v1.Service) []gatewayv1.GatewayStatusAddress {
	ipType, hostType := gatewayv1.IPAddressType, gatewayv1.HostnameAddressType
	var out []gatewayv1.GatewayStatusAddress
	for _, ing := range svc.Status.LoadBalancer.Ingress {
		if ing.IP != "" {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &ipType, Value: ing.IP})
		}
		if ing.Hostname != "" {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &hostType, Value: ing.Hostname})
		}
	}
	if len(out) > 0 {
		return out
	}
	for _, ip := range svc.Spec.ClusterIPs {
		if ip != "" && ip != v1.ClusterIPNone {
			out = append(out, gatewayv1.GatewayStatusAddress{Type: &ipType, Value: ip})
		}
	}
	return out
}

// nginxPortsOf reads the nginx port of every listener port from a Service.
func nginxPortsOf(svc *v1.Service) map[listenerPort]int32 {
	out := make(map[listenerPort]int32)
	if svc == nil {
		return out
	}
	for _, p := range svc.Spec.Ports {
		if p.TargetPort.Type != intstr.Int || p.TargetPort.IntVal <= 0 {
			continue
		}
		proto := p.Protocol
		if proto == "" {
			proto = v1.ProtocolTCP
		}
		out[listenerPort{port: p.Port, protocol: proto}] = p.TargetPort.IntVal
	}
	return out
}

// addressing assigns addresses to the Gateways of one Build.
type addressing struct {
	c *Controller
	// used are the nginx ports taken, by any Gateway's Service
	used map[int32]bool
	// existing are the Services of Gateways, by namespace and name
	existing map[types.NamespacedName]*v1.Service
	// ports are the nginx ports of the Services that passed the checks of
	// validPorts, by Service: only those are served and kept
	ports map[types.NamespacedName]map[listenerPort]int32
	// gateways are the UIDs of the Gateways of the graph, which a Service has
	// to be owned by
	gateways map[types.NamespacedName]types.UID
	// wanted are the Services the Gateways of this Build have
	wanted map[types.NamespacedName]bool
	leader bool
	// endpoints are those of the controller's own Service
	endpoints []discoveryv1.EndpointSlice
}

func (c *Controller) newAddressing(gateways map[types.NamespacedName]types.UID) (*addressing, error) {
	a := &addressing{
		c:        c,
		used:     make(map[int32]bool),
		existing: make(map[types.NamespacedName]*v1.Service),
		ports:    make(map[types.NamespacedName]map[listenerPort]int32),
		gateways: gateways,
		wanted:   make(map[types.NamespacedName]bool),
	}
	if c.cfg.IsLeader != nil {
		a.leader = c.cfg.IsLeader()
	}
	for _, p := range c.cfg.ReservedPorts {
		a.used[p] = true
	}
	svcs, err := c.ownServices()
	if err != nil {
		return nil, err
	}
	for _, svc := range svcs {
		a.existing[types.NamespacedName{Namespace: svc.Namespace, Name: svc.Name}] = svc
	}
	a.validPorts()
	if a.leader {
		a.endpoints = c.controllerEndpoints()
	}
	return a, nil
}

// The Service of a Gateway is in the Gateway's namespace, where its owners
// can change it: the nginx ports read back from it are checked before they
// are served. A port counts only if
//
//   - the Service is the controller's: its controlling owner is the
//     Gateway of the graph it is named after, with that Gateway's UID - a
//     Service made by someone else with the labels is never taken over;
//   - it is a number in -gateway-listener-ports and none of the ports
//     nginx or the controller listen on otherwise;
//   - no other port has it, of this Service or another: of those that do,
//     the one of the Service that was written to first keeps it (the
//     lowest resourceVersion, as one changed by hand is written to last),
//     the others lose theirs.
//
// A port that fails a check is not served, and the leader gives the listener
// a free one in its place.

// gatewayOwner is the Gateway a Service names as its controlling owner.
func gatewayOwner(svc *v1.Service) (types.NamespacedName, types.UID, bool) {
	for _, o := range svc.OwnerReferences {
		if o.Controller == nil || !*o.Controller || o.Kind != "Gateway" ||
			!strings.HasPrefix(o.APIVersion, gatewayv1.GroupName+"/") {
			continue
		}
		return types.NamespacedName{Namespace: svc.Namespace, Name: o.Name}, o.UID, true
	}
	return types.NamespacedName{}, "", false
}

// ownedService tells whether svc is the Service the controller made for the
// Gateway gw of the graph.
func (a *addressing) ownedService(svc *v1.Service, gw types.NamespacedName) bool {
	owner, uid, ok := gatewayOwner(svc)
	if !ok || owner != gw || svc.Name != ServiceName(gw, a.c.cfg.GatewayClass) {
		return false
	}
	want, ok := a.gateways[gw]
	return ok && want == uid
}

// inRange tells whether n may be the nginx port of a listener.
func (a *addressing) inRange(n int32) bool {
	lo, hi := a.c.cfg.ListenerPortMin, a.c.cfg.ListenerPortMax
	if lo <= 0 || hi < lo || n < lo || n > hi {
		return false
	}
	return !slices.Contains(a.c.cfg.ReservedPorts, n)
}

// validPorts checks the nginx ports of the existing Services and keeps those
// that pass in ports and used.
func (a *addressing) validPorts() {
	type claim struct {
		svc     types.NamespacedName
		lp      listenerPort
		rv      uint64
		rvOK    bool
		created metav1.Time
	}
	claims := make(map[int32][]claim)
	for _, name := range slices.SortedFunc(maps.Keys(a.existing), compareNames) {
		svc := a.existing[name]
		gw, _, isGateways := gatewayOwner(svc)
		owned := isGateways && a.ownedService(svc, gw)
		rv, err := strconv.ParseUint(svc.ResourceVersion, 10, 64)
		for lp, n := range nginxPortsOf(svc) {
			if !a.inRange(n) || (lp.protocol != v1.ProtocolTCP && lp.protocol != v1.ProtocolUDP) {
				continue
			}
			if !owned {
				// the Service of a Gateway not in the graph (yet), or of
				// none: its ports are served by no one, but are not given to
				// another Gateway while it is there - the leader deletes it
				if isGateways {
					a.used[n] = true
				}
				continue
			}
			claims[n] = append(claims[n], claim{svc: name, lp: lp, rv: rv, rvOK: err == nil, created: svc.CreationTimestamp})
		}
	}
	for n, cs := range claims {
		winner := slices.MinFunc(cs, func(x, y claim) int {
			switch {
			case x.rvOK && y.rvOK && x.rv != y.rv:
				return cmpUint(x.rv, y.rv)
			case !x.created.Equal(&y.created):
				if x.created.Before(&y.created) {
					return -1
				}
				return 1
			case x.svc != y.svc:
				return compareNames(x.svc, y.svc)
			case x.lp.port != y.lp.port:
				return int(x.lp.port - y.lp.port)
			}
			return strings.Compare(string(x.lp.protocol), string(y.lp.protocol))
		})
		if len(cs) > 1 {
			a.c.logger.Info("nginx port claimed by more than one Gateway listener, kept by the Service written to first",
				"port", n, "service", winner.svc.String(), "listener", winner.lp.portName())
		}
		if a.ports[winner.svc] == nil {
			a.ports[winner.svc] = make(map[listenerPort]int32)
		}
		a.ports[winner.svc][winner.lp] = n
		a.used[n] = true
	}
}

func compareNames(x, y types.NamespacedName) int {
	return strings.Compare(x.String(), y.String())
}

func cmpUint(x, y uint64) int {
	switch {
	case x < y:
		return -1
	case x > y:
		return 1
	}
	return 0
}

// ownServices are the Services of the Gateways of the class, in every
// namespace: those of namespaces no longer watched too, for collect.
func (c *Controller) ownServices() ([]*v1.Service, error) {
	if c.services == nil {
		return nil, nil
	}
	return c.services.List(c.ownServiceSelector())
}

// controllerEndpoints are the EndpointSlices of the controller's own Service.
func (c *Controller) controllerEndpoints() []discoveryv1.EndpointSlice {
	if c.endpointSlices == nil || c.cfg.ExternalService == "" {
		return nil
	}
	slices, err := c.endpointSlices.EndpointSlices(c.cfg.Namespace).List(
		labels.SelectorFromSet(labels.Set{discoveryv1.LabelServiceName: c.cfg.ExternalService}))
	if err != nil {
		return nil
	}
	out := make([]discoveryv1.EndpointSlice, 0, len(slices))
	for _, s := range slices {
		out = append(out, *s)
	}
	return out
}

// address gives gw its address: the controller's if it is the default
// Gateway, else its Service's, which the leader creates or updates first.
func (a *addressing) address(ctx context.Context, gw *graph.Gateway) gatewayAddress {
	name := types.NamespacedName{Namespace: gw.Source.Namespace, Name: gw.Source.Name}
	restrictTLSListeners(gw)
	restrictFrontendValidation(gw)
	if name == a.c.cfg.DefaultGateway {
		restrictDefaultGateway(gw)
		return gatewayAddress{shared: true}
	}

	svcName := types.NamespacedName{Namespace: name.Namespace, Name: ServiceName(name, a.c.cfg.GatewayClass)}
	wanted := wantedPorts(gw)
	if len(wanted) == 0 {
		// no listener to serve, no Service - one without ports is invalid;
		// the listeners' conditions say why
		return gatewayAddress{nginxPorts: map[listenerPort]int32{}}
	}
	a.wanted[svcName] = true

	current := a.existing[svcName]
	if current != nil && !a.ownedService(current, name) {
		// a Service of that name the controller did not make for this
		// Gateway - made by hand, or for a Gateway of the name before: it is
		// neither taken over nor served
		a.c.logger.Info("the Service of a Gateway is not owned by it and is not used", "gateway", name.String(), "service", svcName.String())
		return gatewayAddress{pending: fmt.Sprintf("its Service %s exists but was not made for it by the controller", svcName)}
	}
	if a.leader {
		svc, err := a.ensureService(ctx, gw, svcName, current, wanted)
		if err != nil {
			a.c.logger.Error(err, "making the Service of a Gateway", "gateway", name.String(), "service", svcName.String())
		} else {
			current = svc
			if err := a.ensureEndpointSlices(ctx, svc); err != nil {
				a.c.logger.Error(err, "making the EndpointSlices of a Gateway", "gateway", name.String(), "service", svcName.String())
			}
		}
	}
	if current == nil {
		return gatewayAddress{pending: fmt.Sprintf("its Service %s does not exist yet", svcName)}
	}

	have := a.ports[svcName]
	out := gatewayAddress{nginxPorts: make(map[listenerPort]int32), addresses: serviceAddresses(current)}
	var missing []string
	for _, p := range wanted {
		if n, ok := have[p]; ok {
			out.nginxPorts[p] = n
		} else {
			missing = append(missing, p.portName())
		}
	}
	switch {
	case len(missing) > 0:
		out.pending = fmt.Sprintf("its Service %s has no port %s yet", svcName, strings.Join(missing, ", "))
	case len(out.addresses) == 0:
		out.pending = fmt.Sprintf("its Service %s has no address yet", svcName)
	}
	return out
}

// ensureService creates or updates the Service of a Gateway: a port for
// every listener port, each on the nginx port it had or a free one, no
// selector - its EndpointSlice is the controller's - and the Gateway as its
// owner.
func (a *addressing) ensureService(
	ctx context.Context,
	gw *graph.Gateway,
	name types.NamespacedName,
	current *v1.Service,
	wanted []listenerPort,
) (*v1.Service, error) {
	cfg := a.c.cfg
	have := a.ports[name]
	var ports []v1.ServicePort
	for _, p := range wanted {
		n, ok := have[p]
		if !ok {
			n, ok = a.allocate(gw, p)
			if !ok {
				return nil, fmt.Errorf("no free nginx port left in %d-%d for %s", cfg.ListenerPortMin, cfg.ListenerPortMax, p.portName())
			}
		}
		ports = append(ports, v1.ServicePort{
			Name:       p.portName(),
			Protocol:   p.protocol,
			Port:       p.port,
			TargetPort: intstr.FromInt32(n),
		})
	}

	lbls := map[string]string{}
	annotations := map[string]string{}
	if infra := gw.Source.Spec.Infrastructure; infra != nil {
		for k, v := range infra.Labels {
			lbls[string(k)] = string(v)
		}
		for k, v := range infra.Annotations {
			annotations[string(k)] = string(v)
		}
	}
	lbls[LabelManagedBy] = managedBy
	lbls[LabelGatewayClass] = cfg.GatewayClass
	lbls[LabelGatewayName] = truncateLabel(gw.Source.Name)
	lbls[LabelGatewayNamespace] = gw.Source.Namespace
	annotations[AnnotationGateway] = gw.Source.Namespace + "/" + gw.Source.Name

	svcType := cfg.ServiceType
	if svcType == "" {
		svcType = v1.ServiceTypeLoadBalancer
	}

	owners := []metav1.OwnerReference{{
		APIVersion: gatewayv1.GroupVersion.String(),
		Kind:       "Gateway",
		Name:       gw.Source.Name,
		UID:        gw.Source.UID,
		Controller: &controllerOwner,
	}}

	if current == nil {
		svc := &v1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:            name.Name,
				Namespace:       name.Namespace,
				Labels:          lbls,
				Annotations:     mergeMaps(map[string]string{annotationOwnedMeta: ownedMetaValue(lbls, annotations)}, annotations),
				OwnerReferences: owners,
			},
			Spec: v1.ServiceSpec{
				Type:  svcType,
				Ports: ports,
			},
		}
		a.staticAddresses(gw, svc)
		created, err := cfg.KubeClient.CoreV1().Services(name.Namespace).Create(ctx, svc, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("the Service %s exists but is not in the cache yet", name)
		}
		if err == nil {
			a.ports[name] = portsOfSpec(ports)
		}
		return created, err
	}

	svc := current.DeepCopy()
	// keep the labels and annotations others set, but drop those the
	// controller set and the Gateway's infrastructure no longer has
	for _, k := range strings.Split(current.Annotations[annotationOwnedMeta], ",") {
		if key, ok := strings.CutPrefix(k, "l:"); ok {
			if _, still := lbls[key]; !still {
				delete(svc.Labels, key)
			}
		}
		if key, ok := strings.CutPrefix(k, "a:"); ok {
			if _, still := annotations[key]; !still {
				delete(svc.Annotations, key)
			}
		}
	}
	svc.Labels = mergeMaps(svc.Labels, lbls)
	svc.Annotations = mergeMaps(svc.Annotations, annotations)
	svc.Annotations[annotationOwnedMeta] = ownedMetaValue(lbls, annotations)
	svc.OwnerReferences = owners
	svc.Spec.Selector = nil
	svc.Spec.Type = svcType
	// keep the node ports the API server assigned
	nodePorts := make(map[string]int32)
	for _, p := range current.Spec.Ports {
		nodePorts[p.Name] = p.NodePort
	}
	for i := range ports {
		if svcType != v1.ServiceTypeClusterIP {
			ports[i].NodePort = nodePorts[ports[i].Name]
		}
	}
	svc.Spec.Ports = ports
	a.staticAddresses(gw, svc)
	if equality.Semantic.DeepEqual(current.Labels, svc.Labels) &&
		equality.Semantic.DeepEqual(current.Annotations, svc.Annotations) &&
		equality.Semantic.DeepEqual(current.OwnerReferences, svc.OwnerReferences) &&
		equality.Semantic.DeepEqual(current.Spec, svc.Spec) {
		a.ports[name] = portsOfSpec(ports)
		return current, nil
	}
	updated, err := cfg.KubeClient.CoreV1().Services(name.Namespace).Update(ctx, svc, metav1.UpdateOptions{})
	if err == nil {
		a.ports[name] = portsOfSpec(ports)
	}
	return updated, err
}

// portsOfSpec are the nginx ports of the ports the leader wrote.
func portsOfSpec(ports []v1.ServicePort) map[listenerPort]int32 {
	out := make(map[listenerPort]int32, len(ports))
	for _, p := range ports {
		out[listenerPort{port: p.Port, protocol: p.Protocol}] = p.TargetPort.IntVal
	}
	return out
}

var controllerOwner = true

// staticAddresses is item 3's: the Gateway's spec.addresses on the Service.
func (a *addressing) staticAddresses(_ *graph.Gateway, _ *v1.Service) {}

// endpointSliceManagedBy marks the EndpointSlices of Gateways' Services.
const endpointSliceManagedBy = "gateway.wxingress.wizardtales.com"

// ensureEndpointSlices makes the EndpointSlices of a Gateway's Service: one
// per address family of the controller's endpoints, with their addresses
// and readiness, on the Service's nginx ports.
func (a *addressing) ensureEndpointSlices(ctx context.Context, svc *v1.Service) error {
	var ports []discoveryv1.EndpointPort
	for _, p := range svc.Spec.Ports {
		ports = append(ports, discoveryv1.EndpointPort{
			Name:     ptrTo(p.Name),
			Protocol: ptrTo(p.Protocol),
			Port:     ptrTo(p.TargetPort.IntVal),
		})
	}
	byFamily := make(map[discoveryv1.AddressType][]discoveryv1.Endpoint)
	for _, s := range a.endpoints {
		for _, e := range s.Endpoints {
			byFamily[s.AddressType] = append(byFamily[s.AddressType], discoveryv1.Endpoint{
				Addresses:  e.Addresses,
				Conditions: e.Conditions,
				NodeName:   e.NodeName,
				Zone:       e.Zone,
			})
		}
	}
	if len(byFamily) == 0 {
		// the controller's own Service has no endpoints (yet): keep what is there
		return nil
	}
	owner := []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: svc.Name, UID: svc.UID, Controller: &controllerOwner}}
	client := a.c.cfg.KubeClient.DiscoveryV1().EndpointSlices(svc.Namespace)
	for _, family := range []discoveryv1.AddressType{discoveryv1.AddressTypeIPv4, discoveryv1.AddressTypeIPv6} {
		endpoints := byFamily[family]
		name := svc.Name + "-" + strings.ToLower(string(family))
		if len(endpoints) == 0 {
			continue
		}
		slices.SortFunc(endpoints, func(x, y discoveryv1.Endpoint) int {
			return strings.Compare(strings.Join(x.Addresses, ","), strings.Join(y.Addresses, ","))
		})
		want := &discoveryv1.EndpointSlice{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: svc.Namespace,
				Labels: map[string]string{
					discoveryv1.LabelServiceName: svc.Name,
					discoveryv1.LabelManagedBy:   endpointSliceManagedBy,
					LabelGatewayClass:            a.c.cfg.GatewayClass,
				},
				OwnerReferences: owner,
			},
			AddressType: family,
			Endpoints:   endpoints,
			Ports:       ports,
		}
		key := svc.Namespace + "/" + name
		digest := fmt.Sprintf("%s|%v|%v", svc.UID, endpoints, ports)
		if a.c.slicesWritten[key] == digest {
			continue
		}
		cur, err := client.Get(ctx, name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
			_, err = client.Create(ctx, want, metav1.CreateOptions{})
		case err == nil:
			cur = cur.DeepCopy()
			cur.Labels, cur.OwnerReferences = want.Labels, want.OwnerReferences
			cur.Endpoints, cur.Ports = want.Endpoints, want.Ports
			_, err = client.Update(ctx, cur, metav1.UpdateOptions{})
		}
		if err != nil {
			return err
		}
		a.c.slicesWritten[key] = digest
	}
	return nil
}

func ptrTo[T any](v T) *T { return &v }

// annotationOwnedMeta lists the labels and annotations the controller set on
// a Service, so that it removes those the Gateway's infrastructure no longer
// has and keeps those others set.
const annotationOwnedMeta = "wxingress.wizardtales.com/owned-metadata"

func ownedMetaValue(lbls, annotations map[string]string) string {
	var keys []string
	for k := range lbls {
		keys = append(keys, "l:"+k)
	}
	for k := range annotations {
		keys = append(keys, "a:"+k)
	}
	slices.Sort(keys)
	return strings.Join(keys, ",")
}

func mergeMaps(dst, src map[string]string) map[string]string {
	if dst == nil {
		dst = make(map[string]string, len(src))
	}
	maps.Copy(dst, src)
	return dst
}

func truncateLabel(v string) string {
	if len(v) <= validation.LabelValueMaxLength {
		return v
	}
	return strings.TrimRight(v[:validation.LabelValueMaxLength], "-_.")
}

// allocate picks a free nginx port for a listener port of gw: where a hash of
// both points in the range, or the next free one after it, so that a Gateway
// gets the same ports again if its Service is made anew.
func (a *addressing) allocate(gw *graph.Gateway, p listenerPort) (int32, bool) {
	lo, hi := a.c.cfg.ListenerPortMin, a.c.cfg.ListenerPortMax
	if lo <= 0 || hi < lo {
		return 0, false
	}
	size := int64(hi-lo) + 1
	sum := sha256.Sum256([]byte(gw.Source.Namespace + "/" + gw.Source.Name + "/" + p.portName()))
	start := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(size)) //nolint:gosec // size is positive
	for i := int64(0); i < size; i++ {
		port := lo + int32((start+i)%size) //nolint:gosec // within the range
		if !a.used[port] {
			a.used[port] = true
			return port, true
		}
	}
	return 0, false
}

// collect deletes the Services of Gateways that are no longer of the class,
// no longer have listeners or are in a namespace no longer watched; those of
// deleted Gateways go with them, as their owner, and the EndpointSlices with
// their Service. A Service in a namespace watched but not synced yet stays:
// its Gateway may only not be listed yet. Only the leader deletes.
func (a *addressing) collect(ctx context.Context) {
	if !a.leader {
		return
	}
	for name, svc := range a.existing {
		if a.wanted[name] || !a.c.syncedFor(svc.Namespace) {
			continue
		}
		if _, _, ok := gatewayOwner(svc); !ok {
			// one with the labels that no Gateway owns is not the controller's
			continue
		}
		err := a.c.cfg.KubeClient.CoreV1().Services(svc.Namespace).Delete(ctx, svc.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &svc.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			a.c.logger.Error(err, "deleting the Service of a Gateway that is gone", "service", name.String())
		}
	}
}
